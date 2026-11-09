// Package query implements the read-only query API (/v1/query/*) used by the CLI,
// the MCP server, AI skills and the dashboard.
//
// Authentication: "Authorization: Bearer <token>" with one of query.tokens, or a
// signed-in dashboard session (Handler.Allow). Every SQL statement runs on a
// read-only connection (mode=ro + query_only), so the API cannot modify data.
package query

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/catalog"
	"github.com/RedHiwiK/HiwiKInsight/internal/config"
	"github.com/RedHiwiK/HiwiKInsight/internal/metrics"
	"github.com/RedHiwiK/HiwiKInsight/internal/tz"
)

const (
	sqlTimeout = 10 * time.Second
	maxRows    = 1000
	maxDays    = 400
)

type Handler struct {
	db      *sql.DB
	cfg     *config.Config
	catalog *catalog.Set

	// Allow lets a request through without a bearer token (a signed-in dashboard session).
	Allow func(*http.Request) bool
	// Rates returns exchange rates for the dashboard's display currencies
	// (1 base currency = N units); nil shows the base currency only.
	Rates func() map[string]float64
}

func New(db *sql.DB, cfg *config.Config, cat *catalog.Set) *Handler {
	return &Handler{db: db, cfg: cfg, catalog: cat}
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/query/describe", h.auth(h.describe))
	mux.HandleFunc("POST /v1/query/sql", h.auth(h.sql))
	mux.HandleFunc("GET /v1/query/metric/{name}", h.auth(h.metric))
	mux.HandleFunc("GET /v1/query/user/{id}", h.auth(h.user))
	mux.HandleFunc("GET /v1/query/meta", h.auth(h.meta))
}

func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.Allow != nil && h.Allow(r) {
			next(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok && got != "" {
			for _, tok := range h.cfg.Query.Tokens {
				if tok != "" && subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1 {
					next(w, r)
					return
				}
			}
		}
		writeError(w, http.StatusUnauthorized, "unauthorized")
	}
}

// ---------- SQL ----------

var (
	leadingKeyword = regexp.MustCompile(`(?i)^\s*(select|with)\b`)
	commentRE      = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)
)

// ValidateSQL allows a single SELECT / WITH statement; the read-only connection is the second line of defense.
func ValidateSQL(q string) (string, error) {
	q = strings.TrimSpace(commentRE.ReplaceAllString(q, " "))
	q = strings.TrimSpace(strings.TrimRight(q, "; \n\t"))
	if q == "" {
		return "", errors.New("empty sql")
	}
	if strings.Contains(q, ";") {
		return "", errors.New("only a single statement is allowed")
	}
	if !leadingKeyword.MatchString(q) {
		return "", errors.New("only SELECT / WITH queries are allowed")
	}
	return q, nil
}

type SQLResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
}

func (h *Handler) sql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, `body must be {"sql": "..."}`)
		return
	}
	q, err := ValidateSQL(req.SQL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := RunSQL(r.Context(), h.db, q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// RunSQL runs a query with a timeout and a row limit.
func RunSQL(ctx context.Context, db *sql.DB, q string) (*SQLResult, error) {
	ctx, cancel := context.WithTimeout(ctx, sqlTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	res := &SQLResult{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}

// ---------- metrics ----------

var dayRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// defaultApp is the first configured app.
func (h *Handler) defaultApp() string {
	if len(h.cfg.Apps) > 0 {
		return h.cfg.Apps[0].Key
	}
	return ""
}

func (h *Handler) params(r *http.Request) (metrics.Params, error) {
	q := r.URL.Query()
	today := tz.Today()
	p := metrics.Params{App: q.Get("app"), Env: q.Get("env"), From: q.Get("from"), To: q.Get("to"), Today: today}
	if p.App == "" {
		p.App = h.defaultApp()
	}
	app, ok := h.cfg.AppByKey(p.App)
	if !ok {
		return p, fmt.Errorf("unknown app %q", p.App)
	}
	p.Bundle = app.BundleID
	p.Apps = map[string]string{}
	for _, a := range h.cfg.Apps {
		p.Apps[a.Key] = a.BundleID
	}
	if p.Env == "" {
		p.Env = "production"
	}
	if p.To == "" {
		p.To = today
	}
	if p.From == "" {
		p.From = mustDay(p.To).AddDate(0, 0, -29).Format("2006-01-02")
	}
	if !dayRE.MatchString(p.From) || !dayRE.MatchString(p.To) || p.From > p.To {
		return p, errors.New("from/to must be YYYY-MM-DD and from <= to")
	}
	if mustDay(p.To).Sub(mustDay(p.From)) > maxDays*24*time.Hour {
		return p, fmt.Errorf("range too large (max %d days)", maxDays)
	}
	p.Limit, _ = strconv.Atoi(q.Get("limit"))
	p.Args = map[string]string{}
	for k := range q {
		switch k {
		case "app", "env", "from", "to", "limit":
		default:
			p.Args[k] = q.Get(k)
		}
	}
	return p, nil
}

func mustDay(s string) time.Time {
	t, _ := tz.ParseDay(s)
	return t
}

func (h *Handler) metric(w http.ResponseWriter, r *http.Request) {
	p, err := h.params(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sqlTimeout)
	defer cancel()
	res, err := metrics.Run(ctx, h.db, r.PathValue("name"), p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) user(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	if app == "" {
		app = h.defaultApp()
	}
	if _, ok := h.cfg.AppByKey(app); !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown app %q", app))
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ctx, cancel := context.WithTimeout(r.Context(), sqlTimeout)
	defer cancel()
	res, err := metrics.User(ctx, h.db, app, strings.ToLower(r.PathValue("id")), limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- meta ----------

type appMeta struct {
	Key             string            `json:"key"`
	Name            string            `json:"name"`
	Bundle          string            `json:"bundle"`
	Color           string            `json:"color"`
	Products        map[string]string `json:"products"`
	PaywallContexts map[string]string `json:"paywall_contexts"`
	Modules         [][2]string       `json:"modules"`
	Events          map[string]string `json:"events"`
}

// palette is the order in which apps without a configured color get one.
var palette = []string{"purple", "teal", "orange", "blue", "pink", "violet", "green", "gold"}

// AppColors assigns every app a palette slot: configured colors first, then the
// unused slots in config order.
func AppColors(apps []config.App) map[string]string {
	out, used := map[string]string{}, map[string]bool{}
	for _, a := range apps {
		if a.Color != "" {
			out[a.Key], used[a.Color] = a.Color, true
		}
	}
	var free []string
	for _, c := range palette {
		if !used[c] {
			free = append(free, c)
		}
	}
	i := 0
	for _, a := range apps {
		if out[a.Key] != "" {
			continue
		}
		if len(free) == 0 {
			out[a.Key] = palette[i%len(palette)]
		} else {
			out[a.Key] = free[i%len(free)]
		}
		i++
	}
	return out
}

// meta returns what the dashboard needs: apps (in config order), their modules and
// event descriptions, the reporting day, the base currency and exchange rates.
// The dashboard also uses it to check whether it is signed in.
func (h *Handler) meta(w http.ResponseWriter, r *http.Request) {
	colors := AppColors(h.cfg.Apps)
	apps := []appMeta{}
	for _, a := range h.cfg.Apps {
		apps = append(apps, appMeta{Key: a.Key, Name: a.Name, Bundle: a.BundleID, Color: colors[a.Key],
			Products: orEmpty(a.Products), PaywallContexts: orEmpty(a.PaywallContexts),
			Modules: h.catalog.Modules(a.Key), Events: h.catalog.EventDescs(a.Key)})
	}
	rates := map[string]float64{}
	if h.Rates != nil {
		rates = h.Rates()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apps": apps, "today": tz.Today(), "timezone": h.cfg.Timezone,
		"currency": h.cfg.Currency, "fx": rates,
	})
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// ---------- describe ----------

func (h *Handler) describe(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	fmt.Fprintf(&b, describeIntro, h.cfg.Timezone, h.cfg.Currency, h.cfg.RetentionDays)

	b.WriteString("\n## Apps\n\n| app | bundle_id | name |\n|---|---|---|\n")
	for _, a := range h.cfg.Apps {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", a.Key, a.BundleID, a.Name)
	}

	b.WriteString("\n## Built-in metrics (insight metric <name> --app --env --from --to)\n\n")
	for _, d := range metrics.All() {
		fmt.Fprintf(&b, "- `%s`: %s\n", d.Name, d.Definition)
	}

	b.WriteString("\n## Data on hand\n\n")
	if res, err := RunSQL(r.Context(), h.db, `
		SELECT app, env, COUNT(*) AS events, COUNT(DISTINCT install_id) AS installs,
			MIN(day) AS first_day, MAX(day) AS last_day FROM events GROUP BY app, env`); err == nil {
		b.WriteString(markdownTable(res.Columns, res.Rows))
	}

	b.WriteString("\n## Undocumented events (stored but missing from the catalog; ask the developer what they mean)\n\n")
	if res, err := RunSQL(r.Context(), h.db, `SELECT app, name, COUNT(*) FROM events GROUP BY app, name ORDER BY app, name`); err == nil {
		var missing []string
		for _, row := range res.Rows {
			app, name := fmt.Sprint(row[0]), fmt.Sprint(row[1])
			if !h.catalog.EventNames(app)[name] {
				missing = append(missing, fmt.Sprintf("- %s / %s (%v events)", app, name, row[2]))
			}
		}
		if len(missing) == 0 {
			b.WriteString("None\n")
		} else {
			b.WriteString(strings.Join(missing, "\n") + "\n")
		}
	}

	for _, app := range h.catalog.Apps() {
		fmt.Fprintf(&b, "\n## Event catalog: %s\n\n```yaml\n%s\n```\n", app, strings.TrimRight(h.catalog.Text(app), "\n"))
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(b.String()))
}

func markdownTable(cols []string, rows [][]any) string {
	if len(rows) == 0 {
		return "(no data yet)\n"
	}
	var b strings.Builder
	b.WriteString("| " + strings.Join(cols, " | ") + " |\n|" + strings.Repeat("---|", len(cols)) + "\n")
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, v := range r {
			cells[i] = fmt.Sprint(v)
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// describeIntro is the semantic layer for AI assistants and people. Placeholders:
// time zone, base currency, raw event retention in days.
const describeIntro = `# HiwiKInsight data guide (semantic layer for AI assistants and people)

Self-hosted anonymous app analytics plus App Store transactions, stored in SQLite. Every query is read-only.

## Conventions

- **Time zone**: %[1]s. Every day column in tables and views is a calendar date (YYYY-MM-DD) in this zone. ts / *_at columns are millisecond Unix timestamps; convert them with local_date(ts) or local_datetime(ts), and use local_today() for today
- **Users**: anonymous install_id (one install on one device; reinstalling creates a new id). No personal data or IP addresses are stored
- **Environment env**: production (App Store users; the default and usually the only one to look at) / sandbox (TestFlight) / xcode (development builds)
- **Truly new user**: an install whose first-seen day equals its first App Store download day (acquired_at comes from AppTransaction). People who installed the app before it shipped the SDK are first seen later than their download day and are not new users
- **Active**: had any event that day
- **Session**: returning after more than 5 minutes in the background starts a new session; session.ended is sent on the next launch
- **Parameters**: events.params is a JSON string; read values with json_extract(params, '$.key'). Count-like parameters are bucketed strings (0 / 1 / 2-5 / 6-20 / 21-100 / 100+)
- **Money**: amounts are converted to the base currency %[2]s at the exchange rate of the day they were received
- Raw events are kept for %[3]d days; installs, daily_active and purchase_attempts are kept forever

## Tables

- **events** (raw events): event_id, app, env, install_id, session_id, name, ts, day, received_at, app_version, build, os, os_version, device (model identifier such as iPhone17,1), locale, language, region (system region), storefront (App Store country, ISO alpha-3), appearance, text_size, params
- **installs** (one row per install): app, install_id, env, first_seen, acquired_at, first_version, last_seen, last_version, device, os_version, region, storefront, language (the last columns hold the latest values)
- **daily_active** (user × day): app, env, day, install_id, sessions (session.started that day), duration_s (foreground seconds of sessions that ended that day), events
- **purchase_attempts** (purchases started in the app): id, token (= StoreKit appAccountToken; random per purchase, or the app's own account ID repeated across purchases), app, env, install_id, context (paywall entry point), product, ts, app_version, device, os_version
- **appstore_events** (App Store Server Notifications, real transactions): notification_uuid, bundle_id, environment (Production / Sandbox, mind the case), notification_type, subtype, product_id, transaction_id, storefront, currency, price_milli, revenue_sign (+1 income / -1 refund / 0 other; +1 with price_milli = 0 is free, not a sale; it is a free-trial start only when notification_type = 'SUBSCRIBED'), amount (base currency), signed_at, received_at, app_account_token, app, original_transaction_id, transaction_reason (PURCHASE / RENEWAL), purchase_at, original_purchase_at. Join to the attempt through v_attribution, not on the token
- **asc_sales** (App Store Connect sales reports, if configured): frequency (DAILY / MONTHLY), period, month, bundle_id, sku, product_type, category (download / redownload / update / iap / subscription / other), units (negative for refunds), proceeds (base currency, after Apple's commission and tax), country, subscription (New / Renewal)
- **asc_store_engagement** / **asc_store_downloads** (App Store Connect analytics reports, if configured): impressions, product page views and downloads by date, bundle_id, source, territory
- **device_models**: identifier → name (marketing name of the device model)

## Views (prefer them when writing SQL)

- **v_user_days**: app, env, day, install_id, sessions, duration_s, events
- **v_user_summary**: one row per user: first_seen_day, acquired_day, is_new, last_seen_day, first_version, last_version, device, device_name, os_version, region, storefront, language, active_days, sessions, duration_s, is_paid (a paid App Store sale attributed to this install through appAccountToken), is_pro (current member: is_pro = "true" in the latest user.snapshot, or an App Store purchase or trial start after it that has not since expired or been refunded; also covers members who bought before the SDK was integrated)
- **v_attribution**: one row per App Store notification with an appAccountToken: notification_uuid, attempt_id (= purchase_attempts.id, NULL when unattributed). Use it to link transactions to the paywall attempt: appstore_events a JOIN v_attribution v ON v.notification_uuid = a.notification_uuid JOIN purchase_attempts p ON p.id = v.attempt_id
- **v_sessions**: one row per session: install_id, session_id, started_at, day, source (how the app was opened), duration_s, screen_views, events
- **v_screen_usage**: screen views and exits: day, install_id, session_id, ts, kind (viewed / left), screen, module, duration_s (left only)

## Example queries

` + "```sql" + `
-- How many days each user was active in the last 7 days
SELECT install_id, COUNT(*) AS days FROM v_user_days
WHERE app = 'myapp' AND env = 'production' AND day >= date(local_today(), '-6 days')
GROUP BY install_id ORDER BY days DESC;

-- Users per module in the last 30 days
SELECT module, COUNT(DISTINCT install_id) AS users, COUNT(*) AS views FROM v_screen_usage
WHERE app = 'myapp' AND env = 'production' AND kind = 'viewed' AND day >= date(local_today(), '-29 days')
GROUP BY module ORDER BY users DESC;

-- How sessions start (icon, widget, notification, ...)
SELECT source, COUNT(*) FROM v_sessions WHERE app = 'myapp' AND env = 'production' GROUP BY source;
` + "```\n"
