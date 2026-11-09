package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRecordAndStats(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 9, 23, 15, 0, 0, 0, loc)
	amt := func(v float64) *float64 { return &v }
	ev := func(id string, sign int, amount *float64, at time.Time) Event {
		e := Event{NotificationUUID: id, BundleID: "a", Environment: "Production", NotificationType: "X", RevenueSign: sign, Amount: amount, SignedAt: at.UnixMilli()}
		if sign != 0 {
			e.PriceMilli = 1000
		}
		return e
	}

	// Price-0 income is never a sale; it is a free-trial start only when it starts a subscription
	free := func(id, typ string, at time.Time) Event {
		e := ev(id, 1, amt(0), at)
		e.NotificationType, e.PriceMilli = typ, 0
		return e
	}
	for _, e := range []Event{
		ev("today-1", 1, amt(18), now.Add(-time.Hour)),
		ev("today-2", 1, amt(30), now.Add(-2*time.Hour)),
		ev("today-refund", -1, amt(18), now.Add(-30*time.Minute)),
		ev("today-foreign", 1, nil, now.Add(-10*time.Minute)),
		ev("today-info", 0, nil, now.Add(-5*time.Minute)),
		free("today-trial", "SUBSCRIBED", now.Add(-3*time.Minute)),
		free("today-redeemed", "ONE_TIME_CHARGE", now.Add(-2*time.Minute)),
		func() Event {
			e := free("today-code-sub", "SUBSCRIBED", now.Add(-time.Minute))
			e.OfferType = OfferCode
			return e
		}(),
		ev("month-1", 1, amt(12), now.AddDate(0, 0, -5)),
		ev("last-month", 1, amt(99), now.AddDate(0, -1, 0)),
	} {
		if _, err := s.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	// Apple retrying the same notification is not counted twice
	if inserted, _ := s.Record(ev("today-1", 1, amt(18), now.Add(-time.Hour))); inserted {
		t.Fatal("duplicate notification should be ignored")
	}
	// Other environments are excluded
	sandbox := ev("sandbox", 1, amt(500), now)
	sandbox.Environment = "Sandbox"
	s.Record(sandbox)

	st, err := s.Stats("a", "Production", now, loc)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{
		Today: Period{Sales: 3, Trials: 1, Refunds: 1, Net: 30, Unconverted: 1},
		Month: Period{Sales: 4, Trials: 1, Refunds: 1, Net: 42, Unconverted: 1},
	}
	if st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
}
