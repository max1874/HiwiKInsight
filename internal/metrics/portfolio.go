package metrics

// Business overview across all apps: revenue, downloads and scale of every app.
// Revenue comes from App Store Connect sales reports (proceeds = after commission and taxes, converted to the
// base currency at the exchange rate at fetch time); a month uses the monthly report when available, otherwise the
// sum of daily reports. Reports arrive a day later, so the latest live sales are added from App Store notifications.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/store"
)

func init() {
	defs = append(defs,
		Def{"portfolio", "Portfolio dashboard: proceeds of all apps (App Store Connect sales reports, after commission and taxes, in the base currency) month to date / same period last month / last month / year to date / last 12 months; per-app comparison (proceeds, downloads, MAU); within the selected range (range=week last 7 days / month last 30 days / year last 12 months / custom uses from-to) the trend by day or month x app, revenue mix (one-time purchase / subscription / paid download) and country breakdown; plus the latest live sales feed", portfolio},
	)
}

// Sales rows on a monthly basis: months with a monthly report use it, other months use daily reports.
const salesSrc = `
	WITH monthly_done AS (SELECT period FROM asc_reports WHERE frequency = 'MONTHLY' AND status IN ('ok', 'empty')),
	src AS (
		SELECT * FROM asc_sales WHERE frequency = 'MONTHLY'
		UNION ALL
		SELECT * FROM asc_sales WHERE frequency = 'DAILY' AND month NOT IN (SELECT period FROM monthly_done)
	)`

func portfolio(ctx context.Context, db *sql.DB, p Params) ([]Table, error) {
	var latest sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT MAX(period) FROM asc_reports WHERE frequency = 'DAILY' AND status IN ('ok', 'empty')`).Scan(&latest); err != nil {
		return nil, err
	}
	var earliest sql.NullString
	db.QueryRowContext(ctx, `SELECT MIN(month) FROM asc_sales`).Scan(&earliest)
	status := Table{Name: "status", Columns: []string{"latest_report_day", "earliest_month", "synced"},
		Rows: [][]any{{nullStr(latest), nullStr(earliest), latest.Valid}}}
	if !latest.Valid {
		// No reports synced yet: return only the live feed.
		feed, err := portfolioFeed(ctx, db, 0)
		if err != nil {
			return nil, err
		}
		return []Table{status, feed}, nil
	}
	day := latest.String
	thisMonth := day[:7]
	lastMonth := parseDay(thisMonth+"-01").AddDate(0, -1, 0).Format("2006-01")
	lastMonthSameDay := lastMonth + day[7:]
	if parseDay(lastMonthSameDay).Format("2006-01") != lastMonth { // e.g. 3/31 → 2/28
		lastMonthSameDay = parseDay(thisMonth+"-01").AddDate(0, 0, -1).Format("2006-01-02")
	}
	year := day[:4]
	// Range for the trend, revenue mix and country breakdown: week = last 7 days / month = last 30 days (by day,
	// ending on the latest report day), year = last 12 months (by month, including this month, monthly basis),
	// custom = from-to (daily reports, aggregated by month when longer than 92 days).
	rng := p.arg("range")
	var rangeFrom, rangeTo, gran string
	switch rng {
	case "week", "month":
		n := 7
		if rng == "month" {
			n = 30
		}
		rangeFrom, rangeTo, gran = parseDay(day).AddDate(0, 0, -(n-1)).Format("2006-01-02"), day, "day"
	case "custom":
		rangeFrom, rangeTo, gran = p.From, p.To, "day"
		if parseDay(p.To).Sub(parseDay(p.From)) > 91*24*time.Hour {
			gran = "month"
		}
	default:
		rng = "year"
		rangeFrom, rangeTo, gran = parseDay(thisMonth+"-01").AddDate(0, -11, 0).Format("2006-01"), thisMonth, "month"
	}
	// year filters by month on the monthly basis (monthly report when available); the others filter daily reports by day.
	rangeSrc, rangeWhere, bucket := salesSrc+" SELECT * FROM src", "month BETWEEN ? AND ?", "month"
	if rng != "year" {
		rangeSrc, rangeWhere, bucket = "SELECT * FROM asc_sales WHERE frequency = 'DAILY'", "period BETWEEN ? AND ?", "period"
		if gran == "month" {
			bucket = "month"
		}
	}
	rangeSrc = "WITH r AS (" + rangeSrc + ")"

	// ---- KPI ----
	sumDaily := func(from, to string) (proceeds float64, downloads, paid, refunds float64, err error) {
		err = db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(proceeds), 0),
				COALESCE(SUM(CASE WHEN category = 'download' THEN units END), 0),
				COALESCE(SUM(CASE WHEN category IN ('iap', 'subscription') AND units > 0 THEN units
				                  WHEN category = 'download' AND proceeds_per_unit > 0 AND units > 0 THEN units END), 0),
				COALESCE(SUM(CASE WHEN units < 0 AND proceeds_per_unit != 0 THEN -units END), 0)
			FROM asc_sales WHERE frequency = 'DAILY' AND period BETWEEN ? AND ?`, from, to).Scan(&proceeds, &downloads, &paid, &refunds)
		return
	}
	curP, curD, curPaid, curRef, err := sumDaily(thisMonth+"-01", day)
	if err != nil {
		return nil, err
	}
	prevP, prevD, prevPaid, prevRef, err := sumDaily(lastMonth+"-01", lastMonthSameDay)
	if err != nil {
		return nil, err
	}
	monthTotal := func(from, to string) (float64, error) {
		var v float64
		err := db.QueryRowContext(ctx, salesSrc+` SELECT COALESCE(SUM(proceeds), 0) FROM src WHERE month BETWEEN ? AND ?`, from, to).Scan(&v)
		return v, err
	}
	lastMonthTotal, err := monthTotal(lastMonth, lastMonth)
	if err != nil {
		return nil, err
	}
	ytd, err := monthTotal(year+"-01", thisMonth)
	if err != nil {
		return nil, err
	}
	last12, err := monthTotal(parseDay(thisMonth+"-01").AddDate(0, -11, 0).Format("2006-01"), thisMonth)
	if err != nil {
		return nil, err
	}
	allTime, err := monthTotal("0000-00", "9999-99")
	if err != nil {
		return nil, err
	}
	// Live sales after the latest report (App Store notifications, price converted to the base currency, before commission).
	var live float64
	var liveCount int
	db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(revenue_sign * amount), 0), COALESCE(SUM(price_milli > 0), 0) FROM appstore_events
		WHERE environment = 'Production' AND revenue_sign != 0 AND signed_at > ?`,
		endOfPacificDay(day)).Scan(&live, &liveCount) // sales reports cover whole Pacific Time days

	kpis := Table{Name: "kpis", Columns: []string{"key", "current", "previous"}, Rows: [][]any{
		{"mtd_proceeds", money(curP), money(prevP)},
		{"last_month_proceeds", money(lastMonthTotal), nil},
		{"ytd_proceeds", money(ytd), nil},
		{"last_12m_proceeds", money(last12), nil},
		{"all_time_proceeds", money(allTime), nil},
		{"mtd_downloads", curD, prevD},
		{"mtd_paid_units", curPaid, prevPaid},
		{"mtd_refunds", curRef, prevRef},
		{"live_after_report", money(live), liveCount},
	}}
	period := Table{Name: "period", Columns: []string{"this_month", "through_day", "last_month", "last_month_same_day", "range", "range_from", "range_to", "granularity"},
		Rows: [][]any{{thisMonth, day, lastMonth, lastMonthSameDay, rng, rangeFrom, rangeTo, gran}}}

	// ---- by day / month x app ----
	trend, err := queryTable(ctx, db, "trend", rangeSrc+`
		SELECT `+bucket+` AS bucket, bundle_id, ROUND(COALESCE(SUM(proceeds), 0), 2) AS proceeds,
			COALESCE(SUM(CASE WHEN category = 'download' THEN units END), 0) AS downloads
		FROM r WHERE `+rangeWhere+` GROUP BY 1, bundle_id ORDER BY 1, bundle_id`, rangeFrom, rangeTo)
	if err != nil {
		return nil, err
	}

	// ---- per-app comparison ----
	apps, err := portfolioApps(ctx, db, p, thisMonth, day, lastMonth, lastMonthSameDay)
	if err != nil {
		return nil, err
	}

	// ---- revenue mix (selected range) ----
	structure, err := queryTable(ctx, db, "structure", rangeSrc+`
		SELECT CASE WHEN category = 'subscription' THEN CASE WHEN subscription = 'Renewal' THEN 'subscription_renewal' ELSE 'subscription_new' END
		            WHEN category = 'iap' THEN 'iap'
		            WHEN category = 'download' THEN 'paid_download'
		            ELSE 'other' END AS kind,
			ROUND(COALESCE(SUM(proceeds), 0), 2) AS proceeds,
			COALESCE(SUM(CASE WHEN proceeds_per_unit != 0 THEN units END), 0) AS units
		FROM r WHERE `+rangeWhere+` AND proceeds_per_unit != 0 GROUP BY 1 ORDER BY 2 DESC`, rangeFrom, rangeTo)
	if err != nil {
		return nil, err
	}
	countries, err := queryTable(ctx, db, "countries", rangeSrc+`
		SELECT country, ROUND(COALESCE(SUM(proceeds), 0), 2) AS proceeds,
			COALESCE(SUM(CASE WHEN category = 'download' THEN units END), 0) AS downloads
		FROM r WHERE `+rangeWhere+` GROUP BY country HAVING proceeds != 0 OR downloads > 0
		ORDER BY proceeds DESC, downloads DESC LIMIT 12`, rangeFrom, rangeTo)
	if err != nil {
		return nil, err
	}

	// ---- daily proceeds for the last 30 days (sparkline) ----
	daily, err := queryTable(ctx, db, "daily", `
		SELECT period AS day, ROUND(COALESCE(SUM(proceeds), 0), 2) AS proceeds,
			COALESCE(SUM(CASE WHEN category = 'download' THEN units END), 0) AS downloads
		FROM asc_sales WHERE frequency = 'DAILY' AND period > ? GROUP BY period ORDER BY period`,
		parseDay(day).AddDate(0, 0, -30).Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	daily = fillDays(daily, parseDay(day).AddDate(0, 0, -29).Format("2006-01-02"), day)

	feed, err := portfolioFeed(ctx, db, 0)
	if err != nil {
		return nil, err
	}
	return []Table{status, kpis, period, trend, apps, structure, countries, daily, feed}, nil
}

func money(v float64) float64 { return math.Round(v*100) / 100 }

func nullStr(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

// fillDays fills missing days in a per-day table (other columns set to 0).
func fillDays(t Table, from, to string) Table {
	have := map[string][]any{}
	for _, r := range t.Rows {
		have[fmt.Sprint(r[0])] = r
	}
	out := Table{Name: t.Name, Columns: t.Columns, Rows: [][]any{}}
	for _, d := range dayRange(from, to) {
		if r, ok := have[d]; ok {
			out.Rows = append(out.Rows, r)
			continue
		}
		row := []any{d}
		for i := 1; i < len(t.Columns); i++ {
			row = append(row, 0)
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}

// portfolioApps returns one row per app (apps in the account ∪ apps sending analytics).
func portfolioApps(ctx context.Context, db *sql.DB, p Params, thisMonth, day, lastMonth, lastMonthSameDay string) (Table, error) {
	type row struct {
		bundle, name, key                string
		mtd, prevSame, lastMonth, last12 float64
		dlMTD, dlPrevSame                float64
		mau, newUsers                    any
		analytics                        bool
	}
	rows := map[string]*row{}
	get := func(bundle string) *row {
		if rows[bundle] == nil {
			rows[bundle] = &row{bundle: bundle}
		}
		return rows[bundle]
	}
	r, err := db.QueryContext(ctx, `SELECT bundle_id, name FROM asc_apps`)
	if err != nil {
		return Table{}, err
	}
	for r.Next() {
		var b, n string
		r.Scan(&b, &n)
		get(b).name = n
	}
	r.Close()
	for key, bundle := range p.Apps {
		get(bundle).key = key
	}

	scan := func(q string, args []any, fn func(x *row, v1, v2 float64)) error {
		rs, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var b string
			var v1, v2 float64
			if err := rs.Scan(&b, &v1, &v2); err != nil {
				return err
			}
			if b != "" {
				fn(get(b), v1, v2)
			}
		}
		return rs.Err()
	}
	dailySum := `SELECT bundle_id, COALESCE(SUM(proceeds), 0), COALESCE(SUM(CASE WHEN category = 'download' THEN units END), 0)
		FROM asc_sales WHERE frequency = 'DAILY' AND period BETWEEN ? AND ? GROUP BY bundle_id`
	if err := scan(dailySum, []any{thisMonth + "-01", day}, func(x *row, v1, v2 float64) { x.mtd, x.dlMTD = v1, v2 }); err != nil {
		return Table{}, err
	}
	if err := scan(dailySum, []any{lastMonth + "-01", lastMonthSameDay}, func(x *row, v1, v2 float64) { x.prevSame, x.dlPrevSame = v1, v2 }); err != nil {
		return Table{}, err
	}
	monthSum := salesSrc + ` SELECT bundle_id, COALESCE(SUM(proceeds), 0), 0 FROM src WHERE month BETWEEN ? AND ? GROUP BY bundle_id`
	if err := scan(monthSum, []any{lastMonth, lastMonth}, func(x *row, v1, _ float64) { x.lastMonth = v1 }); err != nil {
		return Table{}, err
	}
	from12 := parseDay(thisMonth+"-01").AddDate(0, -11, 0).Format("2006-01")
	if err := scan(monthSum, []any{from12, thisMonth}, func(x *row, v1, _ float64) { x.last12 = v1 }); err != nil {
		return Table{}, err
	}

	// Analytics: MAU over the last 30 days and truly new users (production).
	today := p.Today
	if today == "" {
		today = day
	}
	for _, x := range rows {
		if x.key == "" {
			continue
		}
		var mau, fresh int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT install_id) FROM daily_active WHERE app = ? AND env = 'production' AND day > ?`,
			x.key, addDays(today, -30)).Scan(&mau); err != nil {
			return Table{}, err
		}
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM installs WHERE app = ? AND env = 'production'
				AND local_date(first_seen) = local_date(acquired_at)
				AND local_date(first_seen) > ?`, x.key, addDays(today, -30)).Scan(&fresh); err != nil {
			return Table{}, err
		}
		var seen int
		db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM installs WHERE app = ? AND env = 'production')`, x.key).Scan(&seen)
		x.analytics = seen > 0
		if x.analytics {
			x.mau, x.newUsers = mau, fresh
		}
	}

	var total12 float64
	for _, x := range rows {
		total12 += x.last12
	}
	list := make([]*row, 0, len(rows))
	for _, x := range rows {
		list = append(list, x)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].last12 != list[j].last12 {
			return list[i].last12 > list[j].last12
		}
		return list[i].bundle < list[j].bundle
	})
	t := Table{Name: "apps", Columns: []string{"bundle_id", "app", "asc_name", "mtd_proceeds", "prev_same_proceeds", "last_month_proceeds",
		"last_12m_proceeds", "share_12m_pct", "mtd_downloads", "prev_same_downloads", "mau_30d", "new_users_30d", "analytics"}}
	for _, x := range list {
		share := any(nil)
		if total12 > 0 {
			share = math.Round(x.last12/total12*1000) / 10
		}
		t.Rows = append(t.Rows, []any{x.bundle, x.key, x.name, money(x.mtd), money(x.prevSame), money(x.lastMonth), money(x.last12), share,
			x.dlMTD, x.dlPrevSame, x.mau, x.newUsers, x.analytics})
	}
	return t, nil
}

// portfolioFeed returns the latest real transactions (App Store Server Notifications, production) signed at or after since (milliseconds).
func portfolioFeed(ctx context.Context, db *sql.DB, since int64) (Table, error) {
	return queryTable(ctx, db, "feed", `
		SELECT local_datetime(signed_at) AS time_local, bundle_id, notification_type AS type, `+store.SubtypeSQL+` AS subtype,
			product_id, revenue_sign, ROUND(amount, 2) AS amount, storefront, COALESCE(price_milli, 0) = 0 AS free, `+store.FreeTrialSQL+` AS trial
		FROM appstore_events a WHERE environment = 'Production' AND notification_type != 'TEST' AND signed_at >= ?
		ORDER BY signed_at DESC LIMIT 30`, since)
}

// pacific is the time zone of App Store Connect sales reports.
var pacific = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.FixedZone("PST", -8*3600)
}()

// endOfPacificDay returns the end of a report day (Pacific Time) as a millisecond timestamp.
func endOfPacificDay(day string) int64 {
	t, _ := time.ParseInLocation("2006-01-02", day, pacific)
	return t.AddDate(0, 0, 1).UnixMilli()
}
