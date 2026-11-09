package metrics

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/store"
)

// Now = 9/23 12:30: yesterday is counted only up to 12:30, so events and sales after that on 9/22 are excluded.
func TestToday(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return time.UnixMilli(ms("2026-09-23", 12) + 30*60*1000) }

	n := 0
	ev := func(name, day string, hour int, params map[string]string) store.AnalyticsEvent {
		n++
		return store.AnalyticsEvent{ID: fmt.Sprint("e", n), Name: name, TS: ms(day, hour), Session: day, Params: params}
	}
	batch := func(install, env string, events ...store.AnalyticsEvent) *store.Batch {
		return &store.Batch{App: "pawprint", InstallID: install, Env: env, Events: events}
	}
	installed := func(day string) store.AnalyticsEvent {
		return ev("app.installed", day, 8, map[string]string{"original_download_ts": fmt.Sprint(ms(day, 8))})
	}
	if _, err := s.WriteBatches([]*store.Batch{
		// A: new today, saw the paywall, hit an error
		batch("A", "production", installed("2026-09-23"), ev("session.started", "2026-09-23", 8, nil),
			ev("paywall.shown", "2026-09-23", 9, nil), ev("error", "2026-09-23", 9, map[string]string{"id": "x"})),
		// B: yesterday morning (counted) and today
		batch("B", "production", installed("2026-09-10"), ev("session.started", "2026-09-22", 10, nil), ev("session.started", "2026-09-23", 11, nil)),
		// C: only yesterday afternoon (after 12:30, not counted)
		batch("C", "production", installed("2026-09-10"), ev("session.started", "2026-09-22", 15, nil)),
		// D: sandbox, ignored
		batch("D", "sandbox", installed("2026-09-23"), ev("session.started", "2026-09-23", 10, nil)),
	}); err != nil {
		t.Fatal(err)
	}
	rec := func(id, typ string, sign int, price int64, amount float64, day string, hour int) {
		t.Helper()
		if _, err := s.Record(store.Event{NotificationUUID: id, BundleID: "com.example.pawprint", Environment: "Production",
			NotificationType: typ, ProductID: "pro", Currency: "CNY", PriceMilli: price,
			RevenueSign: sign, Amount: &amount, SignedAt: ms(day, hour)}); err != nil {
			t.Fatal(err)
		}
	}
	rec("n1", "ONE_TIME_CHARGE", 1, 18000, 18, "2026-09-23", 10)
	rec("n2", "SUBSCRIBED", 1, 0, 0, "2026-09-23", 11)        // free trial
	rec("n5", "ONE_TIME_CHARGE", 1, 0, 0, "2026-09-23", 11)   // redeemed free: neither a trial nor a sale
	rec("n3", "ONE_TIME_CHARGE", 1, 6000, 6, "2026-09-22", 9) // yesterday before 12:30
	rec("n4", "REFUND", -1, 6000, 6, "2026-09-22", 20)

	res, err := Run(context.Background(), s.ReadOnly(), "today", Params{Apps: map[string]string{"pawprint": "com.example.pawprint"}})
	if err != nil {
		t.Fatal(err)
	}
	kpi := map[string][]any{}
	for _, r := range table(t, res, "kpis").Rows {
		kpi[fmt.Sprint(r[0])] = r
	}
	for key, want := range map[string]string{
		"gross": "18 6", "sales": "1 1", "trials": "1 0", "refunds": "0 0",
		"active_users": "2 1", "new_users": "1 0", "sessions": "2 1", "paywall_users": "1 0", "errors": "1 0",
	} {
		if got := fmt.Sprint(kpi[key][1], " ", kpi[key][2]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	// Hourly: today stops at the current hour; yesterday covers the whole day (C at 15:00 included).
	h := map[string][]any{}
	for _, r := range table(t, res, "hourly").Rows {
		h[fmt.Sprint(r[1])] = r
	}
	if fmt.Sprint(h["8"][2], h["15"][2], h["15"][3], h["10"][3]) != "1 <nil> 1 1" {
		t.Errorf("hourly = %v %v %v", h["8"], h["15"], h["10"])
	}
	feed := table(t, res, "feed")
	if got := len(feed.Rows); got != 3 {
		t.Errorf("feed rows = %d, want 3 (today only)", got)
	}
	trial := slices.Index(feed.Columns, "trial")
	trials := 0
	for _, r := range feed.Rows {
		trials += toInt(r[trial])
	}
	if trials != 1 {
		t.Errorf("feed trial rows = %d, want 1 (the redeemed one-time purchase is not a trial)", trials)
	}
}
