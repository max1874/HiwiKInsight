package asc

import (
	"errors"
	"log/slog"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/mailer"
	"github.com/RedHiwiK/HiwiKInsight/internal/store"
)

// Apple's reports follow Pacific Time and are usually available the next morning
var pacific = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.FixedZone("PST", -8*3600)
}()

const (
	dailyBackfill   = 365 // Apple only keeps daily reports for the last 365 days
	monthlyBackfill = 36  // older history comes from monthly reports
	syncEvery       = 6 * time.Hour
	requestGap      = 250 * time.Millisecond // well below the limit of 3600 requests per hour
)

type converter interface {
	ToBase(milli int64, currency string) (float64, bool)
}

type Syncer struct {
	client *Client
	store  *store.Store
	fx     converter
	now    func() time.Time

	// Chat, when set, gets each new daily report's purchases and refunds of apps without a
	// server notification URL (salesMessage); Currency is the base currency they are shown in.
	Chat     mailer.Sender
	Currency string
	Names    map[string]string // config names of apps (by bundle ID) and products, shown instead of App Store names
}

func NewSyncer(c *Client, st *store.Store, fx converter) *Syncer {
	return &Syncer{client: c, store: st, fx: fx, now: time.Now}
}

// Loop syncs once at startup (the first run backfills history: about 400 requests and a few minutes), then every 6 hours.
func (s *Syncer) Loop() {
	for {
		start := time.Now()
		n, err := s.SyncOnce()
		if err != nil {
			slog.Error("asc sync failed", "error", err, "reports", n)
		} else {
			slog.Info("asc sync done", "reports", n, "took", time.Since(start).Round(time.Second).String())
		}
		// Analytics reports (store impressions, product pages, downloads by source) sync separately; a failure does not affect sales reports
		start = time.Now()
		if n, err := s.SyncAnalytics(); err != nil {
			slog.Error("asc analytics sync failed", "error", err, "instances", n)
		} else {
			slog.Info("asc analytics sync done", "instances", n, "took", time.Since(start).Round(time.Second).String())
		}
		time.Sleep(syncEvery)
	}
}

// SyncOnce fetches all missing daily / monthly reports and returns the number of reports stored.
func (s *Syncer) SyncOnce() (int, error) {
	apps, err := s.client.Apps()
	if err != nil {
		return 0, err
	}
	var rows []store.ASCApp
	bySKU, byID := map[string]string{}, map[string]string{}
	names, live := map[string]string{}, map[string]bool{}
	for _, a := range apps {
		rows = append(rows, store.ASCApp{AppleID: a.ID, BundleID: a.BundleID, SKU: a.SKU, Name: a.Name})
		bySKU[a.SKU], byID[a.ID] = a.BundleID, a.BundleID
		names[a.BundleID], live[a.BundleID] = a.Name, a.Notified
	}
	maps.Copy(names, s.Names)
	if err := s.store.UpsertASCApps(rows); err != nil {
		return 0, err
	}
	resolve := func(r SalesRow) string {
		if r.ParentSKU != "" && r.ParentSKU != " " {
			if b, ok := bySKU[r.ParentSKU]; ok {
				return b
			}
		}
		if b, ok := byID[r.AppleID]; ok {
			return b
		}
		return bySKU[r.SKU]
	}

	fetched := 0
	today := s.now().In(pacific)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, pacific)

	done, err := s.store.ASCReportStatus("DAILY")
	if err != nil {
		return 0, err
	}
	backfill := len(done) == 0 // the first sync loads history, which is not news
	for i := 1; i <= dailyBackfill; i++ {
		d := today.AddDate(0, 0, -i)
		period := d.Format("2006-01-02")
		if st := done[period]; st == "ok" || st == "empty" || st == "gone" {
			continue
		}
		recs, ok, err := s.fetch("DAILY", period, i > 3, resolve)
		if errors.Is(err, ErrGone) {
			break // older daily reports are past the retention period too
		}
		if err != nil {
			return fetched, err
		}
		if ok {
			fetched++
		}
		if s.Chat != nil && !backfill && i <= announceDays {
			if subject, body, ok := salesMessage(period, recs, names, live, s.Currency); ok {
				if err := s.Chat.Send(subject, body, ""); err != nil {
					slog.Error("sales message failed", "period", period, "error", err)
				} else {
					slog.Info("sales message sent", "period", period)
				}
			}
		}
	}

	done, err = s.store.ASCReportStatus("MONTHLY")
	if err != nil {
		return fetched, err
	}
	thisMonth := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, pacific)
	for i := 1; i <= monthlyBackfill; i++ {
		m := thisMonth.AddDate(0, -i, 0)
		period := m.Format("2006-01")
		if st := done[period]; st == "ok" || st == "empty" || st == "gone" {
			continue
		}
		// Last month's report is usually generated in the first days of the next month; a 404 for older months means no sales that month
		_, ok, err := s.fetch("MONTHLY", period, i > 1 || today.Day() > 10, resolve)
		if errors.Is(err, ErrGone) {
			break // older monthly reports are past the retention period too
		}
		if err != nil {
			return fetched, err
		}
		if ok {
			fetched++
		}
	}
	return fetched, nil
}

// fetch downloads and stores the report for one period and returns its rows. When settled is true a 404 is recorded as empty (never retried); otherwise it is left for the next run.
func (s *Syncer) fetch(frequency, period string, settled bool, resolve func(SalesRow) string) ([]store.SalesRecord, bool, error) {
	time.Sleep(requestGap)
	rows, err := s.client.Sales(frequency, period)
	if errors.Is(err, ErrGone) {
		// Record it so later syncs never request this period again
		if serr := s.store.ReplaceSales(frequency, period, "gone", nil); serr != nil {
			return nil, false, serr
		}
		return nil, false, ErrGone
	}
	if errors.Is(err, ErrNotFound) {
		if settled {
			return nil, true, s.store.ReplaceSales(frequency, period, "empty", nil)
		}
		return nil, false, nil
	}
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status >= 500 && !settled {
			return nil, false, nil // Apple occasionally returns 5xx while the current report is still being generated
		}
		return nil, false, err
	}
	recs := make([]store.SalesRecord, 0, len(rows))
	for _, r := range rows {
		rec := store.SalesRecord{
			BundleID: resolve(r), SKU: r.SKU, ParentSKU: strings.TrimSpace(r.ParentSKU), Title: r.Title,
			ProductType: r.ProductType, Category: Category(r.ProductType), Units: r.Units,
			ProceedsPerUnit: r.ProceedsPerUnit, ProceedsCurrency: r.ProceedsCur, CustomerPrice: r.CustomerPrice,
			CustomerCurrency: r.CustomerCur, Country: r.Country, AppleID: r.AppleID,
			Subscription: r.Subscription, SubPeriod: r.Period,
		}
		total := r.Units * r.ProceedsPerUnit
		if total == 0 {
			zero := 0.0
			rec.Proceeds = &zero
		} else if amount, ok := s.fx.ToBase(int64(math.Round(total*1000)), r.ProceedsCur); ok {
			v := math.Round(amount*100) / 100
			rec.Proceeds = &v
		}
		recs = append(recs, rec)
	}
	if err := s.store.ReplaceSales(frequency, period, "ok", recs); err != nil {
		return nil, false, err
	}
	return recs, true, nil
}
