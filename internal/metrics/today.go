package metrics

// Today across all apps from real-time sources only (SDK events and App Store notifications, production):
// App Store Connect reports lag about two days and cannot describe today.

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/store"
	"github.com/RedHiwiK/HiwiKInsight/internal/tz"
)

func init() {
	defs = append(defs,
		Def{"today", "Today so far across all apps (production, reporting time zone), each compared with yesterday up to the same time of day: per app and in total, active users (distinct install_ids with any event; the total sums apps), truly new users (first download day = first seen day), sessions (session.started), paywall viewers (distinct users with paywall.shown), error events, and real-time gross from App Store notifications (price in the base currency before commission, refunds subtracted) with sales (price > 0), free-trial starts and refunds; plus active users per hour today and over all of yesterday, and today's App Store notifications", today},
	)
}

// now is replaceable in tests.
var now = time.Now

type todayCounts struct {
	active, fresh, sessions, paywall, errors int
	gross                                    float64
	sales, trials, refunds                   int
}

func today(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	t := now().In(tz.Location())
	start := tz.StartOfDay(t)
	yStart := tz.StartOfDay(start.Add(-time.Hour))
	elapsed := t.Sub(start)
	// [from, to) windows in milliseconds: today so far, and yesterday up to the same time of day.
	cur := [2]int64{start.UnixMilli(), t.UnixMilli() + 1}
	prev := [2]int64{yStart.UnixMilli(), yStart.Add(elapsed).UnixMilli() + 1}

	type app struct {
		key, bundle string
		cur, prev   todayCounts
		analytics   bool
	}
	byBundle := map[string]*app{}
	for key, bundle := range p.Apps {
		byBundle[bundle] = &app{key: key, bundle: bundle, analytics: true}
	}

	events := func(key string, w [2]int64, c *todayCounts) error {
		return db.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT install_id),
				COALESCE(SUM(name = 'session.started'), 0),
				COUNT(DISTINCT CASE WHEN name = 'paywall.shown' THEN install_id END),
				COALESCE(SUM(name = 'error'), 0)
			FROM events WHERE app = ? AND env = 'production' AND ts >= ? AND ts < ?`, key, w[0], w[1]).
			Scan(&c.active, &c.sessions, &c.paywall, &c.errors)
	}
	fresh := func(key string, w [2]int64, c *todayCounts) error {
		return db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM installs WHERE app = ? AND env = 'production'
				AND first_seen >= ? AND first_seen < ? AND local_date(first_seen) = local_date(acquired_at)`, key, w[0], w[1]).Scan(&c.fresh)
	}
	for _, a := range byBundle {
		for _, x := range []struct {
			w [2]int64
			c *todayCounts
		}{{cur, &a.cur}, {prev, &a.prev}} {
			if err := events(a.key, x.w, x.c); err != nil {
				return nil, err
			}
			if err := fresh(a.key, x.w, x.c); err != nil {
				return nil, err
			}
		}
	}

	sumMoney := func(w [2]int64, pick func(*app) *todayCounts) error {
		rs, err := db.QueryContext(ctx, `
			SELECT bundle_id, COALESCE(SUM(revenue_sign * amount), 0),
				COALESCE(SUM(revenue_sign > 0 AND price_milli > 0), 0),
				COALESCE(SUM(`+store.FreeTrialSQL+`), 0),
				COALESCE(SUM(revenue_sign < 0), 0)
			FROM appstore_events WHERE environment = 'Production' AND revenue_sign != 0 AND signed_at >= ? AND signed_at < ?
			GROUP BY bundle_id`, w[0], w[1])
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var b string
			var c todayCounts
			if err := rs.Scan(&b, &c.gross, &c.sales, &c.trials, &c.refunds); err != nil {
				return err
			}
			if byBundle[b] == nil {
				byBundle[b] = &app{bundle: b}
			}
			x := pick(byBundle[b])
			x.gross, x.sales, x.trials, x.refunds = c.gross, c.sales, c.trials, c.refunds
		}
		return rs.Err()
	}
	if err := sumMoney(cur, func(a *app) *todayCounts { return &a.cur }); err != nil {
		return nil, err
	}
	if err := sumMoney(prev, func(a *app) *todayCounts { return &a.prev }); err != nil {
		return nil, err
	}

	list := make([]*app, 0, len(byBundle))
	for _, a := range byBundle {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].bundle < list[j].bundle })

	var tc, tp todayCounts
	apps := Table{Name: "apps", Columns: []string{"bundle_id", "app", "analytics", "active_users", "prev_active_users", "new_users", "prev_new_users",
		"sessions", "prev_sessions", "paywall_users", "sales", "trials", "refunds", "gross", "prev_gross", "errors"}}
	for _, a := range list {
		c, pr := a.cur, a.prev
		apps.Rows = append(apps.Rows, []any{a.bundle, a.key, a.analytics, c.active, pr.active, c.fresh, pr.fresh,
			c.sessions, pr.sessions, c.paywall, c.sales, c.trials, c.refunds, money(c.gross), money(pr.gross), c.errors})
		for _, x := range [][2]*todayCounts{{&tc, &c}, {&tp, &pr}} {
			s, v := x[0], x[1]
			s.active, s.fresh, s.sessions, s.paywall, s.errors = s.active+v.active, s.fresh+v.fresh, s.sessions+v.sessions, s.paywall+v.paywall, s.errors+v.errors
			s.gross, s.sales, s.trials, s.refunds = s.gross+v.gross, s.sales+v.sales, s.trials+v.trials, s.refunds+v.refunds
		}
	}
	kpis := Table{Name: "kpis", Columns: []string{"key", "current", "previous"}, Rows: [][]any{
		{"gross", money(tc.gross), money(tp.gross)},
		{"sales", tc.sales, tp.sales},
		{"trials", tc.trials, tp.trials},
		{"refunds", tc.refunds, tp.refunds},
		{"active_users", tc.active, tp.active},
		{"new_users", tc.fresh, tp.fresh},
		{"sessions", tc.sessions, tp.sessions},
		{"paywall_users", tc.paywall, tp.paywall},
		{"errors", tc.errors, tp.errors},
	}}
	period := Table{Name: "period", Columns: []string{"day", "yesterday", "now_local", "hour"},
		Rows: [][]any{{start.Format("2006-01-02"), yStart.Format("2006-01-02"), t.Format("15:04"), t.Hour()}}}

	// Hourly active users: today per app, yesterday's whole day for the shape of the rest of today.
	hourly := Table{Name: "hourly", Columns: []string{"app", "hour", "today", "yesterday"}, Rows: [][]any{}}
	for _, a := range list {
		if !a.analytics {
			continue
		}
		counts := map[int][2]int{}
		rs, err := db.QueryContext(ctx, `
			SELECT ts >= ?, CAST(substr(local_datetime(ts), 12, 2) AS INTEGER) AS h, COUNT(DISTINCT install_id)
			FROM events WHERE app = ? AND env = 'production' AND ts >= ? AND ts < ?
			GROUP BY 1, 2`, cur[0], a.key, prev[0], cur[1])
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var isToday bool
			var h, n int
			if err := rs.Scan(&isToday, &h, &n); err != nil {
				rs.Close()
				return nil, err
			}
			v := counts[h]
			if isToday {
				v[0] = n
			} else {
				v[1] = n
			}
			counts[h] = v
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return nil, err
		}
		for h := 0; h < 24; h++ {
			var td any
			if h <= t.Hour() {
				td = counts[h][0]
			}
			hourly.Rows = append(hourly.Rows, []any{a.key, h, td, counts[h][1]})
		}
	}

	feed, err := portfolioFeed(ctx, db, cur[0])
	if err != nil {
		return nil, err
	}
	return []Table{period, kpis, apps, hourly, feed}, nil
}
