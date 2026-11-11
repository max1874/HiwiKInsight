// Package server wires every component of HiwiKInsight into one HTTP server:
//
//	POST /v1/events                   SDK event ingestion (HiwiKInsightKit)
//	POST /v1/appstore/notifications   App Store Server Notifications V2
//	     /v1/query/*                  read-only query API (bearer token or dashboard session)
//	     /v1/auth/*                   dashboard sign-in
//	     /v1/admin/*                  report preview and resend (dashboard session)
//	     /dashboard/*                 web dashboard
//	GET  /healthz                     liveness probe
//
// plus background jobs: App Store Connect sync, daily/weekly reports, alerts and the
// raw event retention purge.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/asc"
	"github.com/RedHiwiK/HiwiKInsight/internal/auth"
	"github.com/RedHiwiK/HiwiKInsight/internal/catalog"
	"github.com/RedHiwiK/HiwiKInsight/internal/config"
	"github.com/RedHiwiK/HiwiKInsight/internal/dashboard"
	"github.com/RedHiwiK/HiwiKInsight/internal/feishu"
	"github.com/RedHiwiK/HiwiKInsight/internal/fx"
	"github.com/RedHiwiK/HiwiKInsight/internal/i18n"
	"github.com/RedHiwiK/HiwiKInsight/internal/ingest"
	"github.com/RedHiwiK/HiwiKInsight/internal/mailer"
	"github.com/RedHiwiK/HiwiKInsight/internal/notify"
	"github.com/RedHiwiK/HiwiKInsight/internal/query"
	"github.com/RedHiwiK/HiwiKInsight/internal/report"
	"github.com/RedHiwiK/HiwiKInsight/internal/store"
	"github.com/RedHiwiK/HiwiKInsight/internal/tz"
	"github.com/RedHiwiK/HiwiKInsight/web"
)

// DisplayCurrencies are the currencies the dashboard can switch to (besides the base currency).
var DisplayCurrencies = []string{"USD", "EUR", "JPY", "GBP", "CNY", "AUD", "CAD", "CHF", "HKD", "SGD"}

type Server struct {
	Config  *config.Config
	Store   *store.Store
	Handler http.Handler

	writer   *store.Writer
	sender   mailer.Sender
	chat     mailer.Sender // Feishu or Lark, nil when not configured
	rates    *fx.Rates
	reports  *report.Scheduler
	alerter  *report.Alerter
	ascCfg   asc.Config
	ingest   *ingest.Handler
	appByKey map[string]string // app key → bundle ID
}

// New applies global settings (time zone, language, currency), opens the database and builds the routes.
func New(cfg *config.Config) (*Server, error) {
	if err := tz.Set(cfg.Timezone); err != nil {
		return nil, err
	}
	i18n.Set(cfg.Language)
	report.SetCurrency(cfg.Currency)
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("data_dir: %w", err)
	}
	if *cfg.Dashboard.Enabled && !cfg.Dashboard.InsecureNoAuth && len(cfg.Dashboard.Users) == 0 {
		slog.Warn("dashboard has no users: add dashboard.users (hiwikinsight hash-password) to sign in")
	}
	if cfg.Dashboard.InsecureNoAuth && !isLoopback(cfg.Listen) {
		return nil, errors.New("dashboard.insecure_no_auth requires listen to be a loopback address such as 127.0.0.1:8080")
	}

	paths := map[string]string{}
	for _, a := range cfg.Apps {
		paths[a.Key] = a.Catalog
	}
	cat, err := catalog.Load(paths)
	if err != nil {
		return nil, err
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Server{Config: cfg, Store: st, writer: store.NewWriter(st), sender: mailer.LogSender{},
		rates: fx.New(cfg.Currency), appByKey: map[string]string{}}
	for _, a := range cfg.Apps {
		s.appByKey[a.Key] = a.BundleID
	}
	if cfg.Mail.Enabled() {
		s.sender = &mailer.SMTPMailer{Host: cfg.Mail.SMTPHost, Port: cfg.Mail.SMTPPort, Username: cfg.Mail.Username,
			Password: cfg.Mail.Password, From: cfg.Mail.From, To: cfg.Mail.To}
	}

	au, err := auth.New(cfg.Dashboard, cfg.DataDir)
	if err != nil {
		st.Close()
		return nil, err
	}

	notifier := notify.New(cfg, st, s.sender, s.rates)
	if f := cfg.Feishu; f.Enabled() {
		s.chat = &feishu.Sender{BaseURL: f.BaseURL, AppID: f.AppID, AppSecret: f.AppSecret, OpenID: f.OpenID}
		notifier.Chat = s.chat
	}
	contexts := map[string]string{}
	for _, a := range cfg.Apps {
		for k, v := range a.PaywallContexts {
			contexts[k] = v
		}
	}
	var apps []report.App
	for _, a := range cfg.Apps {
		apps = append(apps, report.App{Key: a.Key, Bundle: a.BundleID, Name: a.Name})
	}
	payment := map[string]bool{}
	for _, id := range cfg.Alerts.PaymentErrors {
		payment[id] = true
	}
	reportCfg := report.Config{
		Apps:        apps,
		TypeLabel:   notify.TypeLabel,
		ProductName: notifier.ProductName,
		ContextLabel: func(c string) string {
			if n := contexts[c]; n != "" {
				return n
			}
			return c
		},
		Alerts: report.AlertRules{SpikeUsers: cfg.Alerts.SpikeUsers, ReportStaleDays: cfg.Alerts.ReportStaleDays,
			SilentHours: cfg.Alerts.SilentHours, PaymentErrors: payment},
	}
	s.reports = &report.Scheduler{DB: st.ReadOnly(), Ledger: st, Sender: s.sender, Config: reportCfg,
		Hour: *cfg.Reports.Hour, Daily: *cfg.Reports.Daily, Weekly: *cfg.Reports.Weekly, Now: time.Now}
	s.alerter = &report.Alerter{DB: st.ReadOnly(), Ledger: st, Sender: s.sender, Config: reportCfg, Now: time.Now}
	s.ascCfg = asc.Config{KeyID: cfg.ASC.KeyID, IssuerID: cfg.ASC.IssuerID, KeyPath: cfg.ASC.KeyPath, VendorNumber: cfg.ASC.VendorNumber}
	s.ingest = ingest.New(s.appByKey, s.writer, cfg.Ingest.DailyCap)

	mux := http.NewServeMux()
	mux.Handle("POST /v1/appstore/notifications", notifier)
	mux.Handle("POST /v1/events", s.ingest)
	q := query.New(st.ReadOnly(), cfg, cat)
	q.Allow = au.Allowed
	q.Rates = func() map[string]float64 { return s.rates.Snapshot(DisplayCurrencies...) }
	q.Register(mux)
	au.Register(mux)
	mux.HandleFunc("GET /v1/admin/report/preview", au.Require(s.reportPreview))
	mux.HandleFunc("POST /v1/admin/report/send", au.Require(s.reportSend))
	if *cfg.Dashboard.Enabled {
		dashboard.New(web.Dist).Register(mux)
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboard/", http.StatusFound)
		})
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	s.Handler = mux
	return s, nil
}

// Start launches the background jobs.
func (s *Server) Start() {
	cfg := s.Config
	go s.purgeLoop()
	if *cfg.Reports.Daily || *cfg.Reports.Weekly {
		go s.reports.Loop()
	}
	if *cfg.Alerts.Enabled {
		go s.alerter.Loop()
	}
	// TestFlight data from development devices that fell back to production is moved to sandbox (idempotent)
	for _, a := range cfg.Apps {
		if n, err := s.Store.ReclassifyMixedInstalls(a.Key); err != nil {
			slog.Error("reclassify mixed installs", "app", a.Key, "error", err)
		} else if n > 0 {
			slog.Info("reclassified dev-device events to sandbox", "app", a.Key, "events", n)
		}
	}
	s.rates.Snapshot(DisplayCurrencies...) // warm the exchange-rate cache
	if !s.ascCfg.Enabled() {
		slog.Info("app_store_connect not configured, sales and analytics report sync disabled")
		return
	}
	c, err := asc.NewClient(s.ascCfg)
	if err != nil {
		slog.Error("app_store_connect disabled", "error", err)
		return
	}
	syncer := asc.NewSyncer(c, s.Store, s.rates)
	syncer.Chat, syncer.Currency, syncer.Names = s.chat, cfg.Currency, map[string]string{}
	for _, a := range cfg.Apps {
		syncer.Names[a.BundleID] = a.Name
		maps.Copy(syncer.Names, a.Products)
	}
	go syncer.Loop()
	// Released builds: production events from builds never released on the App Store are TestFlight data
	releases := asc.NewReleases()
	go releases.Loop(c, s.Store, s.appByKey)
	s.ingest.Released = func(app, version, build string) (bool, bool) {
		return releases.Released(s.appByKey[app], version, build)
	}
}

// ListenAndServe serves until SIGINT/SIGTERM is delivered through ctx, then flushes queued events.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Config.Listen,
		Handler:           s.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	slog.Info("HiwiKInsight listening", "addr", s.Config.Listen, "public_url", s.Config.PublicURL, "apps", len(s.Config.Apps),
		"timezone", s.Config.Timezone, "currency", s.Config.Currency)
	err := srv.ListenAndServe()
	s.writer.Close()
	s.Store.Close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) purgeLoop() {
	keep := time.Duration(s.Config.RetentionDays) * 24 * time.Hour
	for {
		if n, err := s.Store.PurgeEvents(time.Now().Add(-keep)); err != nil {
			slog.Error("purge events failed", "error", err)
		} else if n > 0 {
			slog.Info("purged expired events", "rows", n)
		}
		time.Sleep(24 * time.Hour)
	}
}

// reportPreview renders a report: ?kind=daily|weekly&period=YYYY-MM-DD (default: the latest), format=text for plain text.
func (s *Server) reportPreview(w http.ResponseWriter, r *http.Request) {
	kind := report.Kind(r.URL.Query().Get("kind"))
	if kind != report.Weekly {
		kind = report.Daily
	}
	today := time.Now().In(tz.Location())
	period := r.URL.Query().Get("period")
	if period == "" {
		period = today.AddDate(0, 0, -1).Format("2006-01-02")
		if kind == report.Weekly {
			period = today.AddDate(0, 0, -int((today.Weekday()+6)%7)-7).Format("2006-01-02")
		}
	}
	v, err := report.Build(r.Context(), s.Store.ReadOnly(), s.reports.Config, kind, period, today.Format("2006-01-02"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.URL.Query().Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("Subject: " + v.Subject + "\n\n" + v.Text()))
		return
	}
	html, err := v.HTML()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// reportSend sends a report now (not recorded, so the scheduled send is unaffected):
// ?kind=daily|weekly&period=YYYY-MM-DD
func (s *Server) reportSend(w http.ResponseWriter, r *http.Request) {
	kind, period := report.Kind(r.URL.Query().Get("kind")), r.URL.Query().Get("period")
	if (kind != report.Daily && kind != report.Weekly) || period == "" {
		http.Error(w, "kind=daily|weekly and period=YYYY-MM-DD are required", http.StatusBadRequest)
		return
	}
	if err := s.reports.SendNow(kind, period); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte("sent\n"))
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
