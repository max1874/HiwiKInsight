// Package metrics defines the built-in metrics (their definitions and computation),
// shared by the query API and the dashboard.
// Dates are always YYYY-MM-DD in the reporting time zone; users are identified by an anonymous install_id.
package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/store"
	"github.com/RedHiwiK/HiwiKInsight/internal/tz"
)

type Params struct {
	App    string // app key, e.g. pawprint
	Bundle string // matching bundle ID, used by revenue metrics
	Env    string // production / sandbox / xcode
	From   string // first day (inclusive)
	To     string // last day (inclusive)
	Today  string // used to decide which retention cells are not yet observable
	Limit  int
	Args   map[string]string // extra per-metric parameters (e.g. event / group_by for event_trend)
	Apps   map[string]string // every app sending analytics: key → bundle ID (used by the portfolio dashboard)
}

type Table struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

type Result struct {
	Metric     string  `json:"metric"`
	Definition string  `json:"definition"`
	App        string  `json:"app"`
	Env        string  `json:"env"`
	From       string  `json:"from"`
	To         string  `json:"to"`
	Tables     []Table `json:"tables"`
}

type Def struct {
	Name       string
	Definition string
	fn         func(ctx context.Context, db *sql.DB, p Params) ([]Table, error)
}

var defs = []Def{
	{"active_users", "Daily DAU (distinct install_ids with any event that day), WAU (trailing 7 days including the day), MAU (trailing 30 days), stickiness DAU/MAU, and the number of truly new users that day (first download day = first seen day)", activeUsers},
	{"activity_days", "As of the `to` day: distribution of how many days each user was active in the last 7 days (L7) and last 28 days (L28); answers \"do users come back every day?\"", activityDays},
	{"retention", "Retention of truly new users grouped by first seen day (by the Monday of the first seen week when cohort=week): Dn = share active on exactly day n after the first seen day; within_7d / within_30d = share that came back at least once within n days; cells not yet observable are empty", retention},
	{"returning", "Daily active users split into new / returning (not first seen that day) / resurrected (inactive for >= 7 consecutive days before); plus, as of `to`, churned (last active >= 14 days ago) and at-risk (7-13 days) user counts", returning},
	{"module_usage", "screen.viewed by module / screen: coverage = users who used the module / active users in the range; views per user; average time on screen in seconds (from screen.left). The features table counts feature-usage events carrying a module parameter (e.g. AI analysis, OCR) by module / event / feature: users, uses and success rate", moduleUsage},
	{"funnel", "Paywall funnel split by entry context: distinct users and conversion rates for paywall.shown -> paywall.cta_tap -> purchase.started -> purchase.success; verified_sales = sales confirmed by the App Store server (price > 0), verified_trials = free-trial starts (subscriptions started at price 0 without an offer code, not counted as sales)", funnel},
	{"revenue", "Actual revenue from App Store Server Notifications (in the base currency) split by entry context, product and storefront, plus the distribution of days from install to purchase; context comes from appAccountToken attribution, missing attribution is recorded as unattributed; sales only counts transactions with price > 0, free-trial starts (subscriptions started at price 0 without an offer code) are listed separately as trials; daily is aggregated per day (reporting time zone) and only contains days with transactions", revenue},
	{"distribution", "Distribution of active users in the range by device model, iOS version, app version, region, App Store storefront and language (latest attributes of each user)", distribution},
}

func All() []Def { return defs }

func Run(ctx context.Context, db *sql.DB, name string, p Params) (*Result, error) {
	for _, d := range defs {
		if d.Name == name {
			tables, err := d.fn(ctx, db, p)
			if err != nil {
				return nil, err
			}
			return &Result{Metric: name, Definition: d.Definition, App: p.App, Env: p.Env, From: p.From, To: p.To, Tables: tables}, nil
		}
	}
	return nil, fmt.Errorf("unknown metric %q", name)
}

// ---------- date helpers ----------

func parseDay(s string) time.Time {
	t, _ := tz.ParseDay(s)
	return t
}

func addDays(day string, n int) string { return parseDay(day).AddDate(0, 0, n).Format("2006-01-02") }

// dayDiff rounds because a day is 23 or 25 hours long across daylight-saving changes.
func dayDiff(from, to string) int {
	return int(math.Round(parseDay(to).Sub(parseDay(from)).Hours() / 24))
}

func dayRange(from, to string) []string {
	var out []string
	for d := from; d <= to; d = addDays(d, 1) {
		out = append(out, d)
	}
	return out
}

func pct(n, d int) any {
	if d == 0 {
		return nil
	}
	return float64(int(float64(n)/float64(d)*1000+0.5)) / 10 // percentage, 1 decimal place
}

// ---------- data loading ----------

// userDays returns each user's active days within [from, to] (ascending).
func userDays(ctx context.Context, db *sql.DB, p Params, from, to string) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT install_id, day FROM daily_active
		WHERE app = ? AND env = ? AND day BETWEEN ? AND ? ORDER BY install_id, day`, p.App, p.Env, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, day string
		if err := rows.Scan(&id, &day); err != nil {
			return nil, err
		}
		out[id] = append(out[id], day)
	}
	return out, rows.Err()
}

func activeByDay(users map[string][]string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for id, days := range users {
		for _, d := range days {
			if out[d] == nil {
				out[d] = map[string]bool{}
			}
			out[d][id] = true
		}
	}
	return out
}

// newUsers returns truly new users (first download day = first seen day) as install_id → first seen day.
func newUsers(ctx context.Context, db *sql.DB, p Params, from, to string) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT install_id, local_date(first_seen) AS d FROM installs
		WHERE app = ? AND env = ?
			AND local_date(first_seen) = local_date(acquired_at)
			AND d BETWEEN ? AND ?`, p.App, p.Env, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, d string
		if err := rows.Scan(&id, &d); err != nil {
			return nil, err
		}
		out[id] = d
	}
	return out, rows.Err()
}

// queryTable converts the result of any SELECT into a Table.
func queryTable(ctx context.Context, db *sql.DB, name, q string, args ...any) (Table, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return Table{}, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	t := Table{Name: name, Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return Table{}, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		t.Rows = append(t.Rows, vals)
	}
	return t, rows.Err()
}

// ---------- metrics ----------

func activeUsers(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	users, err := userDays(ctx, db, p, addDays(p.From, -29), p.To)
	if err != nil {
		return nil, err
	}
	byDay := activeByDay(users)
	fresh, err := newUsers(ctx, db, p, p.From, p.To)
	if err != nil {
		return nil, err
	}
	newByDay := map[string]int{}
	for _, d := range fresh {
		newByDay[d]++
	}
	union := func(end string, n int) int {
		set := map[string]bool{}
		for i := 0; i < n; i++ {
			for id := range byDay[addDays(end, -i)] {
				set[id] = true
			}
		}
		return len(set)
	}
	t := Table{Name: "daily", Columns: []string{"day", "dau", "wau", "mau", "new_users", "stickiness_pct"}}
	sumDAU, days := 0, 0
	for _, d := range dayRange(p.From, p.To) {
		dau, mau := len(byDay[d]), union(d, 30)
		t.Rows = append(t.Rows, []any{d, dau, union(d, 7), mau, newByDay[d], pct(dau, mau)})
		sumDAU += dau
		days++
	}
	summary := Table{Name: "summary", Columns: []string{"days", "avg_dau", "users_in_range", "new_users"},
		Rows: [][]any{{days, float64(sumDAU*10/max(days, 1)) / 10, unionRange(byDay, p.From, p.To), len(fresh)}}}
	return []Table{summary, t}, nil
}

func unionRange(byDay map[string]map[string]bool, from, to string) int {
	set := map[string]bool{}
	for _, d := range dayRange(from, to) {
		for id := range byDay[d] {
			set[id] = true
		}
	}
	return len(set)
}

func activityDays(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	users, err := userDays(ctx, db, p, addDays(p.To, -27), p.To)
	if err != nil {
		return nil, err
	}
	l7, l28 := map[int]int{}, map[int]int{}
	l7Total := 0
	l7Start := addDays(p.To, -6)
	for _, days := range users {
		n7 := 0
		for _, d := range days {
			if d >= l7Start {
				n7++
			}
		}
		if n7 > 0 {
			l7[n7]++
			l7Total++
		}
		l28[len(days)]++
	}
	t7 := Table{Name: "l7", Columns: []string{"active_days_in_7", "users", "share_pct"}}
	for n := 1; n <= 7; n++ {
		t7.Rows = append(t7.Rows, []any{n, l7[n], pct(l7[n], l7Total)})
	}
	buckets := []struct {
		label  string
		lo, hi int
	}{{"1", 1, 1}, {"2-3", 2, 3}, {"4-7", 4, 7}, {"8-14", 8, 14}, {"15-27", 15, 27}, {"28", 28, 28}}
	t28 := Table{Name: "l28", Columns: []string{"active_days_in_28", "users", "share_pct"}}
	for _, b := range buckets {
		n := 0
		for k, v := range l28 {
			if k >= b.lo && k <= b.hi {
				n += v
			}
		}
		t28.Rows = append(t28.Rows, []any{b.label, n, pct(n, len(users))})
	}
	return []Table{t7, t28}, nil
}

var retentionDays = []int{1, 3, 7, 14, 30}

func retention(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	cohort, err := newUsers(ctx, db, p, p.From, p.To)
	if err != nil {
		return nil, err
	}
	users, err := userDays(ctx, db, p, p.From, addDays(p.To, 30))
	if err != nil {
		return nil, err
	}
	type agg struct {
		size        int
		dn          map[int]int
		observable  map[int]int // users whose observation day has arrived (denominator)
		within7     int
		within30    int
		obs7, obs30 int
	}
	byCohort := map[string]*agg{}
	total := &agg{dn: map[int]int{}, observable: map[int]int{}}
	for id, start := range cohort {
		key := start
		if p.Args["cohort"] == "week" { // group by week (Monday)
			t := parseDay(start)
			key = t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7)).Format("2006-01-02")
		}
		a := byCohort[key]
		if a == nil {
			a = &agg{dn: map[int]int{}, observable: map[int]int{}}
			byCohort[key] = a
		}
		active := map[int]bool{}
		for _, d := range users[id] {
			active[dayDiff(start, d)] = true
		}
		for _, x := range []*agg{a, total} {
			x.size++
			for _, n := range retentionDays {
				if addDays(start, n) <= p.Today {
					x.observable[n]++
					if active[n] {
						x.dn[n]++
					}
				}
			}
			within := func(n int) bool {
				for k := 1; k <= n; k++ {
					if active[k] {
						return true
					}
				}
				return false
			}
			if addDays(start, 7) <= p.Today {
				x.obs7++
				if within(7) {
					x.within7++
				}
			}
			if addDays(start, 30) <= p.Today {
				x.obs30++
				if within(30) {
					x.within30++
				}
			}
		}
	}
	cols := []string{"cohort_day", "new_users"}
	for _, n := range retentionDays {
		cols = append(cols, fmt.Sprintf("d%d_pct", n))
	}
	cols = append(cols, "within_7d_pct", "within_30d_pct")
	row := func(label string, a *agg) []any {
		r := []any{label, a.size}
		for _, n := range retentionDays {
			if a.observable[n] == 0 {
				r = append(r, nil)
			} else {
				r = append(r, pct(a.dn[n], a.observable[n]))
			}
		}
		return append(r, pct(a.within7, a.obs7), pct(a.within30, a.obs30))
	}
	t := Table{Name: "cohorts", Columns: cols}
	keys := make([]string, 0, len(byCohort))
	for k := range byCohort {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Rows = append(t.Rows, row(k, byCohort[k]))
	}
	t.Rows = append(t.Rows, row("all", total))
	return []Table{t}, nil
}

func returning(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	users, err := userDays(ctx, db, p, "0000-00-00", p.To)
	if err != nil {
		return nil, err
	}
	daily := Table{Name: "daily", Columns: []string{"day", "active", "new", "returning", "resurrected"}}
	type counts struct{ active, fresh, ret, res int }
	byDay := map[string]*counts{}
	for _, d := range dayRange(p.From, p.To) {
		byDay[d] = &counts{}
	}
	churned, atRisk, resurrectedUsers := 0, 0, map[string]bool{}
	for id, days := range users {
		for i, d := range days {
			c := byDay[d]
			if c == nil {
				continue
			}
			c.active++
			if i == 0 {
				c.fresh++
				continue
			}
			c.ret++
			if dayDiff(days[i-1], d) >= 8 { // at least 7 full days absent in between
				c.res++
				resurrectedUsers[id] = true
			}
		}
		gap := dayDiff(days[len(days)-1], p.To)
		switch {
		case gap >= 14:
			churned++
		case gap >= 7:
			atRisk++
		}
	}
	for _, d := range dayRange(p.From, p.To) {
		c := byDay[d]
		daily.Rows = append(daily.Rows, []any{d, c.active, c.fresh, c.ret, c.res})
	}
	summary := Table{Name: "summary", Columns: []string{"users_ever", "resurrected_users_in_range", "at_risk_7_13d", "churned_14d_plus"},
		Rows: [][]any{{len(users), len(resurrectedUsers), atRisk, churned}}}
	return []Table{summary, daily}, nil
}

func moduleUsage(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	var activeUsers int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT install_id) FROM daily_active WHERE app = ? AND env = ? AND day BETWEEN ? AND ?`,
		p.App, p.Env, p.From, p.To).Scan(&activeUsers); err != nil {
		return nil, err
	}
	const q = `
		WITH v AS (
			SELECT %[1]s AS k, COUNT(*) AS views, COUNT(DISTINCT install_id) AS users
			FROM v_screen_usage WHERE app = ? AND env = ? AND day BETWEEN ? AND ? AND kind = 'viewed' GROUP BY %[1]s
		), l AS (
			SELECT %[1]s AS k, ROUND(AVG(duration_s), 1) AS avg_duration_s
			FROM v_screen_usage WHERE app = ? AND env = ? AND day BETWEEN ? AND ? AND kind = 'left' GROUP BY %[1]s
		)
		SELECT %[2]s v.users, ROUND(v.users * 100.0 / ?, 1) AS coverage_pct, v.views,
			ROUND(v.views * 1.0 / v.users, 1) AS views_per_user, l.avg_duration_s
		FROM v LEFT JOIN l ON l.k = v.k ORDER BY v.users DESC, v.views DESC`
	args := []any{p.App, p.Env, p.From, p.To, p.App, p.Env, p.From, p.To, max(activeUsers, 1)}
	mod, err := queryTable(ctx, db, "modules", fmt.Sprintf(q, "module", "v.k AS module,"), args...)
	if err != nil {
		return nil, err
	}
	scr, err := queryTable(ctx, db, "screens", fmt.Sprintf(q, "module || '/' || screen", "v.k AS module_screen,"), args...)
	if err != nil {
		return nil, err
	}
	// Feature usage: business events carrying a module parameter (AI analysis, OCR, generated reviews, ...),
	// shown next to screen views to see "how often features were used".
	// feature is the first non-empty of feature / source / action; success rate only counts events with a result.
	feat, err := queryTable(ctx, db, "features", `
		SELECT params ->> '$.module' AS module, name AS event,
			COALESCE(params ->> '$.feature', params ->> '$.source', params ->> '$.action', '') AS feature,
			COUNT(DISTINCT install_id) AS users, ROUND(COUNT(DISTINCT install_id) * 100.0 / ?, 1) AS coverage_pct,
			COUNT(*) AS uses, ROUND(COUNT(*) * 1.0 / COUNT(DISTINCT install_id), 1) AS uses_per_user,
			ROUND(SUM(params ->> '$.result' = 'success') * 100.0 / NULLIF(SUM(params ->> '$.result' IS NOT NULL), 0), 1) AS success_pct
		FROM events
		WHERE app = ? AND env = ? AND day BETWEEN ? AND ?
			AND params ->> '$.module' IS NOT NULL AND name NOT IN ('screen.viewed', 'screen.left')
		GROUP BY 1, 2, 3 ORDER BY module, uses DESC`,
		max(activeUsers, 1), p.App, p.Env, p.From, p.To)
	if err != nil {
		return nil, err
	}
	summary := Table{Name: "summary", Columns: []string{"active_users"}, Rows: [][]any{{activeUsers}}}
	return []Table{summary, mod, scr, feat}, nil
}

func funnel(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	t, err := queryTable(ctx, db, "by_context", `
		WITH s AS (
			SELECT COALESCE(json_extract(params, '$.context'), 'unknown') AS context, name, COUNT(DISTINCT install_id) AS users
			FROM events
			WHERE app = ? AND env = ? AND day BETWEEN ? AND ?
				AND name IN ('paywall.shown', 'paywall.cta_tap', 'purchase.started', 'purchase.success')
			GROUP BY 1, 2
		), sales AS (
			SELECT pa.context, SUM(a.price_milli > 0) AS n, SUM(`+store.FreeTrialSQL+`) AS trials
			FROM appstore_events a
			JOIN v_attribution v ON v.notification_uuid = a.notification_uuid
			JOIN purchase_attempts pa ON pa.id = v.attempt_id
			WHERE pa.app = ? AND a.revenue_sign = 1 AND pa.env = ? AND local_date(a.signed_at) BETWEEN ? AND ?
			GROUP BY 1
		), c AS (SELECT context FROM s UNION SELECT context FROM sales)
		SELECT c.context,
			COALESCE(MAX(CASE WHEN s.name = 'paywall.shown' THEN s.users END), 0) AS shown_users,
			COALESCE(MAX(CASE WHEN s.name = 'paywall.cta_tap' THEN s.users END), 0) AS cta_users,
			COALESCE(MAX(CASE WHEN s.name = 'purchase.started' THEN s.users END), 0) AS started_users,
			COALESCE(MAX(CASE WHEN s.name = 'purchase.success' THEN s.users END), 0) AS success_users,
			COALESCE((SELECT n FROM sales WHERE sales.context = c.context), 0) AS verified_sales,
			COALESCE((SELECT trials FROM sales WHERE sales.context = c.context), 0) AS verified_trials
		FROM c LEFT JOIN s ON s.context = c.context
		GROUP BY c.context ORDER BY shown_users DESC`,
		p.App, p.Env, p.From, p.To, p.App, p.Env, p.From, p.To)
	if err != nil {
		return nil, err
	}
	t.Columns = append(t.Columns, "cta_rate_pct", "success_rate_pct")
	for i, r := range t.Rows {
		shown, cta, success := toInt(r[1]), toInt(r[2]), toInt(r[4])
		t.Rows[i] = append(r, pct(cta, shown), pct(success, shown))
	}
	return []Table{t}, nil
}

func toInt(v any) int {
	switch x := v.(type) {
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

// appstore_events.environment uses Apple's capitalization.
var appleEnv = map[string]string{"production": "Production", "sandbox": "Sandbox", "xcode": "Xcode"}

func revenue(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	base := `
		WITH r AS (
			SELECT a.*, COALESCE(NULLIF(pa.context, ''), 'unattributed') AS context, pa.install_id, pa.ts AS attempt_ts
			FROM appstore_events a
			LEFT JOIN v_attribution v ON v.notification_uuid = a.notification_uuid
			LEFT JOIN purchase_attempts pa ON pa.id = v.attempt_id
			WHERE a.bundle_id = ? AND a.environment = ? AND a.revenue_sign != 0
				AND local_date(a.signed_at) BETWEEN ? AND ?
		)`
	args := []any{p.Bundle, appleEnv[p.Env], p.From, p.To}
	// Sales only count transactions with price > 0; free-trial starts are listed as trials, not sales.
	agg := `SUM(revenue_sign = 1 AND price_milli > 0) AS sales, SUM(` + store.FreeTrialSQL + `) AS trials,
		SUM(revenue_sign = -1) AS refunds,
		ROUND(COALESCE(SUM(revenue_sign * amount), 0), 2) AS net, SUM(amount IS NULL AND COALESCE(price_milli, 0) != 0) AS unconverted`
	var tables []Table
	for _, g := range []struct{ name, col, order string }{
		{"summary", "'all'", "net DESC"}, {"by_context", "context", "net DESC"}, {"by_product", "product_id", "net DESC"},
		{"by_storefront", "storefront", "net DESC"},
		// Daily trend: only days with transactions (reporting time zone); callers fill missing days with 0.
		{"daily", "local_date(signed_at)", "key"},
	} {
		t, err := queryTable(ctx, db, g.name, base+fmt.Sprintf(`SELECT %s AS key, %s FROM r GROUP BY 1 ORDER BY %s`, g.col, agg, g.order), args...)
		if err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	t, err := queryTable(ctx, db, "days_to_purchase", base+`
		SELECT CASE
				WHEN d <= 1 THEN '1' WHEN d <= 3 THEN '2-3' WHEN d <= 7 THEN '4-7'
				WHEN d <= 30 THEN '8-30' ELSE '31+' END AS install_day, COUNT(*) AS sales
		FROM (
			SELECT julianday(local_date(r.attempt_ts)) - julianday(local_date(i.acquired_at)) + 1 AS d
			FROM r JOIN installs i ON i.app = ? AND i.install_id = r.install_id
			WHERE r.revenue_sign = 1 AND r.price_milli > 0 AND r.notification_type IN ('ONE_TIME_CHARGE', 'SUBSCRIBED')
		) GROUP BY 1 ORDER BY MIN(d)`, append(args, p.App)...)
	if err != nil {
		return nil, err
	}
	return append(tables, t), nil
}

func distribution(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	var tables []Table
	for _, dim := range []struct{ name, expr string }{
		{"device", "COALESCE(m.name, i.device)"},
		{"os_version", "i.os_version"},
		{"app_version", "i.last_version"},
		{"region", "i.region"},
		{"storefront", "i.storefront"},
		{"language", "i.language"},
	} {
		t, err := queryTable(ctx, db, dim.name, fmt.Sprintf(`
			WITH act AS (SELECT DISTINCT install_id FROM daily_active WHERE app = ? AND env = ? AND day BETWEEN ? AND ?)
			SELECT COALESCE(%s, '(unknown)') AS value, COUNT(*) AS users,
				ROUND(COUNT(*) * 100.0 / (SELECT MAX(COUNT(*), 1) FROM act), 1) AS share_pct
			FROM installs i JOIN act ON act.install_id = i.install_id
			LEFT JOIN device_models m ON m.identifier = i.device
			WHERE i.app = ? GROUP BY 1 ORDER BY users DESC`, dim.expr),
			p.App, p.Env, p.From, p.To, p.App)
		if err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	return tables, nil
}

// User returns one user's summary, activity calendar and recent events (newest first).
func User(ctx context.Context, db *sql.DB, app, installID string, limit int) (*Result, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	summary, err := queryTable(ctx, db, "summary", `SELECT * FROM v_user_summary WHERE app = ? AND install_id = ?`, app, installID)
	if err != nil {
		return nil, err
	}
	calendar, err := queryTable(ctx, db, "calendar", `
		SELECT day, env, sessions, duration_s, events FROM daily_active
		WHERE app = ? AND install_id = ? ORDER BY day DESC`, app, installID)
	if err != nil {
		return nil, err
	}
	events, err := queryTable(ctx, db, "events", `
		SELECT local_datetime(ts) AS time_local, name, substr(session_id, 1, 8) AS session, params, app_version
		FROM events WHERE app = ? AND install_id = ? ORDER BY ts DESC LIMIT ?`, app, installID, limit)
	if err != nil {
		return nil, err
	}
	return &Result{Metric: "user", Definition: "Summary, activity calendar and recent events of a single anonymous user (times in the reporting time zone, newest first)",
		App: app, Tables: []Table{summary, calendar, events}}, nil
}
