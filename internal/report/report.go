// Package report builds and sends the daily and weekly report emails (HTML + plain text).
//
// The daily report covers the previous day: real-time gross revenue and transactions (App Store Server
// Notifications), activity and purchases per app, downloads and proceeds of the newest App Store Connect
// report day (about 2 days behind), and errors first seen that day.
// The weekly report goes out on Mondays for the previous week (Monday to Sunday): proceeds, activity and
// new users, new-user retention, paywall funnel, store conversion and the top 5 errors.
package report

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/i18n"
	"github.com/RedHiwiK/HiwiKInsight/internal/metrics"
	"github.com/RedHiwiK/HiwiKInsight/internal/store"
	"github.com/RedHiwiK/HiwiKInsight/internal/tz"
)

type Kind string

const (
	Daily  Kind = "daily"
	Weekly Kind = "weekly"
)

// App is one app included in the reports.
type App struct {
	Key    string // analytics app key, e.g. pawprint
	Bundle string
	Name   string // display name
}

// Config provides the app list and the display labels (transaction type, product name and paywall entry point
// use the same wording as the transaction notification emails).
type Config struct {
	Apps         []App
	TypeLabel    func(notificationType, subtype string) string
	ProductName  func(productID string) string
	ContextLabel func(context string) string
	Alerts       AlertRules
}

// AlertRules are the thresholds of the alert checks.
type AlertRules struct {
	SpikeUsers      int             // an existing error affecting this many users within an hour alerts
	ReportStaleDays int             // newest App Store Connect daily report older than this alerts
	SilentHours     int             // an app silent for this long (after a week with data) alerts
	PaymentErrors   map[string]bool // error ids that alert on the first occurrence
}

// currency is the base currency of all amounts; set once at startup with SetCurrency.
var currency = "USD"

// SetCurrency sets the base currency used to format amounts.
func SetCurrency(c string) { currency = c }

// Cell is one value in a table or stat card; Delta is the change from the previous period, Up / Down pick the color.
type Cell struct {
	Value string
	Delta string
	Up    bool
	Down  bool
}

type Table struct {
	Title   string
	Note    string
	Columns []string
	Rows    [][]Cell
}

type Stat struct {
	Label string
	Cell
}

// View is the whole content of one report email.
type View struct {
	Subject  string
	Kicker   string // "Daily Report", "Weekly Report"
	Title    string // date / date range
	Hero     Stat
	HeroNote string
	Stats    []Stat
	Tables   []Table
	Footer   string // the default report footer when empty
}

// Build builds the report of one period. period is the day for a daily report and any day of the week for a
// weekly one; today is the sending day (retention observability depends on it).
func Build(ctx context.Context, db *sql.DB, cfg Config, kind Kind, period, today string) (*View, error) {
	b := &builder{ctx: ctx, db: db, cfg: cfg, today: today}
	if err := b.loadSince(); err != nil {
		return nil, err
	}
	if kind == Weekly {
		t := parseDay(period)
		return b.weekly(addDays(period, -int((t.Weekday()+6)%7))) // align to Monday
	}
	return b.daily(period)
}

type builder struct {
	ctx   context.Context
	db    *sql.DB
	cfg   Config
	today string
	// first day of analytics data per app and of real-time transaction notifications: when the previous
	// period starts earlier, no change is shown, avoiding fake growth such as "+71"
	since     map[string]string
	flowSince string
}

func (b *builder) loadSince() error {
	b.since = map[string]string{}
	rs, err := b.db.QueryContext(b.ctx, `SELECT app, MIN(day) FROM daily_active WHERE env = 'production' GROUP BY app`)
	if err != nil {
		return err
	}
	defer rs.Close()
	for rs.Next() {
		var app, day string
		if err := rs.Scan(&app, &day); err != nil {
			return err
		}
		b.since[app] = day
	}
	if err := rs.Err(); err != nil {
		return err
	}
	var flow sql.NullString
	if err := b.db.QueryRowContext(b.ctx, `SELECT MIN(local_date(signed_at)) FROM appstore_events
		WHERE environment = 'Production' AND revenue_sign != 0`).Scan(&flow); err != nil {
		return err
	}
	b.flowSince = flow.String
	return nil
}

// comparable reports whether the app already had analytics data on prevFrom, the start of the previous period.
func (b *builder) comparable(a App, prevFrom string) bool {
	d, ok := b.since[a.Key]
	return ok && d <= prevFrom
}

func (b *builder) flowComparable(prevFrom string) bool {
	return b.flowSince != "" && b.flowSince <= prevFrom
}

// plain drops the change.
func plain(c Cell) Cell { return Cell{Value: c.Value} }

func maybe(c Cell, ok bool) Cell {
	if ok {
		return c
	}
	return plain(c)
}

// ---------- daily ----------

func (b *builder) daily(day string) (*View, error) {
	v := &View{Kicker: i18n.T("Daily Report", "运营日报"), Title: dayLabel(day, true)}
	var dau, prevDAU, fresh, prevFresh, sessions, prevSessions int
	var flow float64
	allComparable := true
	apps := Table{Title: i18n.T("Apps", "各 App"), Columns: []string{"App", "DAU", i18n.T("New users", "真新增"),
		i18n.T("Sessions", "会话"), i18n.T("Paywall → Sales", "付费墙 → 成交"), i18n.T("Gross", "流水")}}
	for _, a := range b.cfg.Apps {
		ok := b.comparable(a, addDays(day, -1))
		allComparable = allComparable && ok
		p := b.params(a, day, day)
		ov, err := metrics.Run(b.ctx, b.db, "overview", p)
		if err != nil {
			return nil, fmt.Errorf("overview %s: %w", a.Key, err)
		}
		k := kpis(ov)
		fn, err := b.funnelTotals(p)
		if err != nil {
			return nil, err
		}
		d, pd := toInt(k["avg_dau"][0]), toInt(k["avg_dau"][1])
		n, pn := toInt(k["new_users"][0]), toInt(k["new_users"][1])
		s, ps := toInt(k["sessions"][0]), toInt(k["sessions"][1])
		f := toFloat(k["net"][0])
		dau, prevDAU, fresh, prevFresh = dau+d, prevDAU+pd, fresh+n, prevFresh+pn
		sessions, prevSessions, flow = sessions+s, prevSessions+ps, flow+f
		apps.Rows = append(apps.Rows, []Cell{
			{Value: a.Name}, maybe(countCell(d, pd), ok), maybe(countCell(n, pn), ok), maybe(countCell(s, ps), ok),
			{Value: people(fn.shown) + " → " + units(fn.sales, "sale", "sales", "笔")}, {Value: money(f)},
		})
	}

	txs, err := b.transactions(day)
	if err != nil {
		return nil, err
	}
	sales, refunds, trials := 0, 0, 0
	for _, t := range txs {
		switch {
		case t.sign < 0:
			refunds++
		case t.priceMilli == 0:
			trials++
		default:
			sales++
		}
	}
	v.Hero = Stat{Label: i18n.T("Yesterday's gross", "昨日流水"), Cell: Cell{Value: money(flow)}}
	v.HeroNote = joinNonEmpty(" · ", units(sales, "sale", "sales", "笔成交"), plural(refunds, "refund", "refunds", "笔退款"),
		plural(trials, "trial started", "trials started", "笔试用开通"),
		i18n.T("App Store real-time notifications, before commission", "App Store 实时通知，未扣佣金"))
	v.Stats = []Stat{
		{Label: i18n.T("Total DAU", "DAU 合计"), Cell: maybe(countCell(dau, prevDAU), allComparable)},
		{Label: i18n.T("New users", "真新增"), Cell: maybe(countCell(fresh, prevFresh), allComparable)},
		{Label: i18n.T("Sessions", "会话"), Cell: maybe(countCell(sessions, prevSessions), allComparable)},
	}
	apps.Note = i18n.T("Changes are vs. the previous day (gross is not compared); DAU is summed across apps", "变化为与前一天相比（流水不比较）；各 App 的 DAU 直接相加")
	v.Tables = append(v.Tables, apps)

	if len(txs) > 0 {
		t := Table{Title: i18n.T("Transactions", "成交明细"), Columns: []string{i18n.T("Time", "时间"), "App", i18n.T("Item", "内容"), i18n.T("Amount", "金额")}}
		for _, x := range txs {
			t.Rows = append(t.Rows, []Cell{{Value: x.time}, {Value: x.app}, {Value: x.label}, x.amount()})
		}
		v.Tables = append(v.Tables, t)
	}

	store, err := b.storeDay(day)
	if err != nil {
		return nil, err
	}
	if store != nil {
		v.Tables = append(v.Tables, *store)
	}
	errs, err := b.newErrors(day)
	if err != nil {
		return nil, err
	}
	if errs != nil {
		v.Tables = append(v.Tables, *errs)
	}
	v.Subject = fmt.Sprintf(i18n.T("Daily Report %s · Gross %s · DAU %d", "运营日报 %s · 流水 %s · DAU %d"), dayLabel(day, false), money(flow), dau)
	return v, nil
}

// ---------- weekly ----------

func (b *builder) weekly(monday string) (*View, error) {
	sunday := addDays(monday, 6)
	v := &View{Kicker: i18n.T("Weekly Report", "运营周报"), Title: rangeLabel(monday, sunday)}
	var avgDAU, prevAvgDAU float64
	var active, prevActive, fresh, prevFresh int
	var flow, prevFlow float64
	prevMonday := addDays(monday, -7)
	allComparable, flowOK := true, b.flowComparable(prevMonday)
	// The headline and the table both use App Store Connect proceeds (after commission and tax); the table splits
	// them by app and adds up to the headline. When the reports do not cover this week yet, both fall back to
	// real-time gross.
	proceeds, prevProceeds, through, mtd, err := b.weekProceeds(monday, sunday)
	if err != nil {
		return nil, err
	}
	var appProceeds, prevAppProceeds map[string]sales
	moneyCol := i18n.T("Gross", "流水")
	if through != "" {
		n := dayDiff(monday, through)
		if appProceeds, err = b.salesByApp(monday, through); err != nil {
			return nil, err
		}
		if prevAppProceeds, err = b.salesByApp(prevMonday, addDays(monday, n-7)); err != nil {
			return nil, err
		}
		moneyCol = i18n.T("Proceeds", "到手")
	}
	apps := Table{Title: i18n.T("Apps", "各 App"),
		Note:    i18n.T("Changes are vs. the previous week; stickiness = avg DAU ÷ MAU", "变化为与前一周相比；粘性 = 日均 DAU ÷ 月活"),
		Columns: []string{"App", i18n.T("Avg DAU", "日均 DAU"), i18n.T("WAU", "周活"), i18n.T("New users", "真新增"), i18n.T("Stickiness", "粘性"), moneyCol}}
	prevWeek := rangeLabel(addDays(monday, -7), addDays(monday, -1))
	retention := Table{Title: i18n.T("New-user retention", "新用户留存"),
		Note:    i18n.T("New users of the previous week ("+prevWeek+")", "前一周（"+prevWeek+"）的真新用户"),
		Columns: []string{"App", i18n.T("New users", "新用户"), i18n.T("Day 1", "次日留存"), i18n.T("Day 7", "7 日留存")}}
	paywall := Table{Title: i18n.T("Paywall funnel", "付费漏斗"),
		Note: i18n.T("Unique users summed across entry points; sales are paid transactions confirmed by the App Store",
			"按入口累计的去重人数；成交为 App Store 确认的付费笔数"),
		Columns: []string{"App", i18n.T("Paywall views", "付费墙曝光"), i18n.T("Tapped buy", "点击购买"), i18n.T("Sales", "成交"), i18n.T("Trials", "试用开通")}}
	var errRows []errRow
	for _, a := range b.cfg.Apps {
		ok := b.comparable(a, prevMonday)
		allComparable = allComparable && ok
		p := b.params(a, monday, sunday)
		ov, err := metrics.Run(b.ctx, b.db, "overview", p)
		if err != nil {
			return nil, fmt.Errorf("overview %s: %w", a.Key, err)
		}
		k := kpis(ov)
		d, pd := toFloat(k["avg_dau"][0]), toFloat(k["avg_dau"][1])
		act, pact := toInt(k["active_users"][0]), toInt(k["active_users"][1])
		n, pn := toInt(k["new_users"][0]), toInt(k["new_users"][1])
		f, pf := toFloat(k["net"][0]), toFloat(k["net"][1])
		avgDAU, prevAvgDAU, active, prevActive = avgDAU+d, prevAvgDAU+pd, active+act, prevActive+pact
		fresh, prevFresh, flow, prevFlow = fresh+n, prevFresh+pn, flow+f, prevFlow+pf
		money := maybe(moneyCell(f, pf), flowOK)
		if through != "" {
			money = moneyCell(appProceeds[a.Bundle].proceeds, prevAppProceeds[a.Bundle].proceeds)
		}
		apps.Rows = append(apps.Rows, []Cell{
			{Value: a.Name}, maybe(decimalCell(d, pd), ok), maybe(countCell(act, pact), ok), maybe(countCell(n, pn), ok),
			{Value: fmt.Sprintf("%.1f%%", toFloat(k["stickiness_pct"][0]))}, money,
		})

		rp := b.params(a, addDays(monday, -7), addDays(monday, -1))
		rt, err := metrics.Run(b.ctx, b.db, "retention", rp)
		if err != nil {
			return nil, fmt.Errorf("retention %s: %w", a.Key, err)
		}
		if row := findRow(rt, "cohorts", "cohort_day", "all"); row != nil && toInt(row["new_users"]) > 0 {
			size := toInt(row["new_users"])
			retention.Rows = append(retention.Rows, []Cell{{Value: a.Name}, {Value: fmt.Sprint(size)},
				{Value: retained(row["d1_pct"], size)}, {Value: retained(row["d7_pct"], size)}})
		}

		fn, err := b.funnelTotals(p)
		if err != nil {
			return nil, err
		}
		if fn.shown+fn.sales+fn.trials > 0 {
			paywall.Rows = append(paywall.Rows, []Cell{{Value: a.Name}, {Value: fmt.Sprint(fn.shown)},
				{Value: fmt.Sprint(fn.cta)}, {Value: fmt.Sprint(fn.sales)}, {Value: fmt.Sprint(fn.trials)}})
		}

		er, err := metrics.Run(b.ctx, b.db, "errors", p)
		if err != nil {
			return nil, fmt.Errorf("errors %s: %w", a.Key, err)
		}
		for _, r := range rows(er, "errors") {
			errRows = append(errRows, errRow{app: a.Name, id: str(r["id"]), count: toInt(r["count"]),
				users: toInt(r["users"]), message: str(r["last_message"])})
		}
	}

	if through != "" {
		v.Hero = Stat{Label: i18n.T("Last week's proceeds", "上周到手收入"), Cell: moneyCell(proceeds, prevProceeds)}
		note := i18n.T("App Store Connect, after commission and tax", "App Store Connect，已扣佣金与税")
		if through < sunday {
			note += i18n.T(" · data through ", " · 数据截至 ") + dayLabel(through, false)
		}
		live := ""
		if b.flowSince != "" {
			since := dayLabel(b.flowSince, false)
			live = i18n.T("Real-time gross "+money(flow)+" (App Store notifications at list price, since "+since+")",
				"实时流水 "+money(flow)+"（App Store 通知原价，"+since+" 起接入）")
		}
		v.HeroNote = joinNonEmpty(" · ", note, i18n.T("Month to date ", "本月至今 ")+money(mtd), live)
		v.Subject = fmt.Sprintf(i18n.T("Weekly Report %s · Proceeds %s · WAU %d", "运营周报 %s · 到手 %s · 周活 %d"), rangeLabel(monday, sunday), money(proceeds), active)
	} else {
		v.Hero = Stat{Label: i18n.T("Last week's gross", "上周流水"), Cell: maybe(moneyCell(flow, prevFlow), flowOK)}
		v.HeroNote = i18n.T("App Store real-time notifications, before commission; App Store Connect reports do not cover this week yet",
			"App Store 实时通知，未扣佣金；App Store Connect 报表尚未覆盖本周")
		v.Subject = fmt.Sprintf(i18n.T("Weekly Report %s · Gross %s · WAU %d", "运营周报 %s · 流水 %s · 周活 %d"), rangeLabel(monday, sunday), money(flow), active)
	}
	v.Stats = []Stat{
		{Label: i18n.T("Weekly active users", "周活跃用户"), Cell: maybe(countCell(active, prevActive), allComparable)},
		{Label: i18n.T("New users", "真新增"), Cell: maybe(countCell(fresh, prevFresh), allComparable)},
		{Label: i18n.T("Avg DAU", "日均 DAU"), Cell: maybe(decimalCell(avgDAU, prevAvgDAU), allComparable)},
	}
	v.Tables = append(v.Tables, apps)
	if len(retention.Rows) > 0 {
		v.Tables = append(v.Tables, retention)
	}
	if len(paywall.Rows) > 0 {
		v.Tables = append(v.Tables, paywall)
	}
	store, err := b.storeWeek(monday, sunday)
	if err != nil {
		return nil, err
	}
	if store != nil {
		v.Tables = append(v.Tables, *store)
	}
	if len(errRows) > 0 {
		sort.SliceStable(errRows, func(i, j int) bool { return errRows[i].count > errRows[j].count })
		t := Table{Title: i18n.T("Top 5 errors", "错误 Top 5"), Columns: errorColumns()}
		for i, e := range errRows {
			if i == 5 {
				break
			}
			t.Rows = append(t.Rows, []Cell{{Value: e.app}, {Value: joinNonEmpty(" · ", e.id, e.message)},
				{Value: fmt.Sprint(e.count)}, {Value: fmt.Sprint(e.users)}})
		}
		v.Tables = append(v.Tables, t)
	}
	return v, nil
}

type errRow struct {
	app, id, message string
	count, users     int
}

// ---------- data ----------

func (b *builder) params(a App, from, to string) metrics.Params {
	return metrics.Params{App: a.Key, Bundle: a.Bundle, Env: "production", From: from, To: to, Today: b.today}
}

type funnelTotal struct{ shown, cta, sales, trials int }

// funnelTotals adds up the funnels of all entry points.
func (b *builder) funnelTotals(p metrics.Params) (funnelTotal, error) {
	res, err := metrics.Run(b.ctx, b.db, "funnel", p)
	if err != nil {
		return funnelTotal{}, fmt.Errorf("funnel %s: %w", p.App, err)
	}
	var t funnelTotal
	for _, r := range rows(res, "by_context") {
		t.shown += toInt(r["shown_users"])
		t.cta += toInt(r["cta_users"])
		t.sales += toInt(r["verified_sales"])
		t.trials += toInt(r["verified_trials"])
	}
	return t, nil
}

type transaction struct {
	time, app, label string
	sign             int
	priceMilli       int64
	value            sql.NullFloat64 // base currency
	currency         string
}

func (t transaction) amount() Cell {
	switch {
	case t.sign > 0 && t.priceMilli == 0:
		return Cell{Value: i18n.T("Trial", "试用")}
	case !t.value.Valid:
		return Cell{Value: fmt.Sprintf("%.2f %s", float64(t.priceMilli)/1000, t.currency)}
	case t.sign < 0:
		return Cell{Value: "-" + money(t.value.Float64), Down: true}
	default:
		return Cell{Value: "+" + money(t.value.Float64), Up: true}
	}
}

// transactions returns the production money notifications of a day (reporting time zone), with the attributed paywall entry point.
func (b *builder) transactions(day string) ([]transaction, error) {
	names := map[string]string{}
	for _, a := range b.cfg.Apps {
		names[a.Bundle] = a.Name
	}
	rs, err := b.db.QueryContext(b.ctx, `
		SELECT a.bundle_id, a.notification_type, `+store.SubtypeSQL+`, a.product_id, a.revenue_sign, a.price_milli, a.amount, a.currency,
			a.signed_at, COALESCE(pa.context, '')
		FROM appstore_events a
		LEFT JOIN v_attribution v ON v.notification_uuid = a.notification_uuid
		LEFT JOIN purchase_attempts pa ON pa.id = v.attempt_id
		WHERE a.environment = 'Production' AND a.revenue_sign != 0
			AND local_date(a.signed_at) = ?
		ORDER BY a.signed_at`, day)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []transaction
	for rs.Next() {
		var bundle, typ, sub, product, context string
		var t transaction
		var signed int64
		if err := rs.Scan(&bundle, &typ, &sub, &product, &t.sign, &t.priceMilli, &t.value, &t.currency, &signed, &context); err != nil {
			return nil, err
		}
		t.time = time.UnixMilli(signed).In(tz.Location()).Format("15:04")
		t.app = orDefault(names[bundle], bundle)
		label := b.cfg.TypeLabel(typ, sub) + " · " + b.cfg.ProductName(product)
		if context != "" {
			label += paren(b.cfg.ContextLabel(context))
		}
		t.label = label
		out = append(out, t)
	}
	return out, rs.Err()
}

// storeDay returns first downloads and proceeds per app for the newest App Store Connect sales report day not
// after day; downloads are compared with the day before it, proceeds are not.
func (b *builder) storeDay(day string) (*Table, error) {
	var latest sql.NullString
	if err := b.db.QueryRowContext(b.ctx, `SELECT MAX(period) FROM asc_sales WHERE frequency = 'DAILY' AND period <= ?`, day).Scan(&latest); err != nil {
		return nil, err
	}
	if !latest.Valid {
		return nil, nil
	}
	cur, err := b.salesByApp(latest.String, latest.String)
	if err != nil {
		return nil, err
	}
	prevDay := addDays(latest.String, -1)
	prev, err := b.salesByApp(prevDay, prevDay)
	if err != nil {
		return nil, err
	}
	t := &Table{Title: i18n.T("App Store · ", "商店 · ") + dayLabel(latest.String, false),
		Columns: []string{"App", i18n.T("First downloads", "首次下载"), i18n.T("Proceeds", "到手收入")},
		Note: i18n.T("App Store Connect sales report (Pacific time, about 2 days behind); proceeds are after commission and tax; download changes are vs. the previous day",
			"App Store Connect 销售报表（太平洋时间，约滞后 2 天），到手已扣佣金与税；下载变化为与前一天相比")}
	for _, a := range b.cfg.Apps {
		c, p := cur[a.Bundle], prev[a.Bundle]
		t.Rows = append(t.Rows, []Cell{{Value: a.Name}, countCell(c.downloads, p.downloads), {Value: money(c.proceeds)}})
	}
	return t, nil
}

type sales struct {
	downloads int
	proceeds  float64
}

func (b *builder) salesByApp(from, to string) (map[string]sales, error) {
	rs, err := b.db.QueryContext(b.ctx, `
		SELECT bundle_id, COALESCE(SUM(CASE WHEN category = 'download' THEN units END), 0), COALESCE(SUM(proceeds), 0)
		FROM asc_sales WHERE frequency = 'DAILY' AND period BETWEEN ? AND ? GROUP BY bundle_id`, from, to)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]sales{}
	for rs.Next() {
		var bundle string
		var d, p float64
		if err := rs.Scan(&bundle, &d, &p); err != nil {
			return nil, err
		}
		out[bundle] = sales{downloads: int(math.Round(d)), proceeds: p}
	}
	return out, rs.Err()
}

// weekProceeds returns this week's proceeds (over the days the reports cover), the same days of the previous
// week, the last covered day and the month-to-date total through that day.
func (b *builder) weekProceeds(monday, sunday string) (cur, prev float64, through string, mtd float64, err error) {
	var latest sql.NullString
	if err = b.db.QueryRowContext(b.ctx, `SELECT MAX(period) FROM asc_sales WHERE frequency = 'DAILY' AND period <= ?`, sunday).Scan(&latest); err != nil {
		return
	}
	if !latest.Valid || latest.String < monday {
		return
	}
	through = latest.String
	n := dayDiff(monday, through)
	total := func(from, to string) (float64, error) {
		m, err := b.salesByApp(from, to)
		var sum float64
		for _, s := range m {
			sum += s.proceeds
		}
		return sum, err
	}
	if cur, err = total(monday, through); err != nil {
		return
	}
	if prev, err = total(addDays(monday, -7), addDays(monday, n-7)); err != nil {
		return
	}
	mtd, err = total(through[:8]+"01", through)
	return
}

// storeWeek returns store conversion (impressions → first downloads) over the 7 days ending at the earlier of
// this week's end and the newest report day.
func (b *builder) storeWeek(monday, sunday string) (*Table, error) {
	res, err := metrics.Run(b.ctx, b.db, "store_funnel", metrics.Params{Env: "production", From: monday, To: sunday,
		Args: map[string]string{"scope": "all"}})
	if err != nil {
		return nil, fmt.Errorf("store_funnel: %w", err)
	}
	win := rows(res, "window")
	if len(win) == 0 {
		return nil, nil
	}
	from, to, complete := str(win[0]["from"]), str(win[0]["to"]), win[0]["prev_complete"] == true
	byBundle := map[string]map[string]any{}
	for _, r := range rows(res, "apps") {
		byBundle[str(r["bundle_id"])] = r
	}
	win7 := rangeLabel(from, to)
	t := &Table{Title: i18n.T("Store conversion", "商店转化"),
		Columns: []string{"App", i18n.T("Unique impressions", "去重曝光"), i18n.T("First downloads", "首次下载"), i18n.T("Conversion", "转化率")},
		Note: i18n.T("App Store Connect analytics report "+win7+"; conversion = first downloads ÷ unique impressions; changes are vs. the previous 7 days",
			"App Store Connect 分析报表 "+win7+"；转化率 = 首次下载 ÷ 去重曝光；变化为与前 7 天相比")}
	for _, a := range b.cfg.Apps {
		r := byBundle[a.Bundle]
		if r == nil {
			continue
		}
		dl := Cell{Value: fmt.Sprint(toInt(r["first_downloads"]))}
		cvr := Cell{Value: fmt.Sprintf("%.1f%%", toFloat(r["cvr"]))}
		if complete {
			dl = countCell(toInt(r["first_downloads"]), toInt(r["prev_first_downloads"]))
			cvr = pctCell(toFloat(r["cvr"]), toFloat(r["prev_cvr"]))
		}
		t.Rows = append(t.Rows, []Cell{{Value: a.Name}, {Value: fmt.Sprint(toInt(r["impressions_unique"]))}, dl, cvr})
	}
	if len(t.Rows) == 0 {
		return nil, nil
	}
	return t, nil
}

// newErrors lists error ids first reported on day (never reported before).
func (b *builder) newErrors(day string) (*Table, error) {
	t := &Table{Title: i18n.T("New errors", "新出现的错误"), Note: i18n.T("Error ids first reported on this day", "当天首次上报的错误标识"), Columns: errorColumns()}
	for _, a := range b.cfg.Apps {
		rs, err := b.db.QueryContext(b.ctx, `
			SELECT json_extract(params, '$.id') AS id, COUNT(*), COUNT(DISTINCT install_id), MAX(json_extract(params, '$.message'))
			FROM events e
			WHERE app = ? AND env = 'production' AND name = 'error' AND day = ?
				AND NOT EXISTS (SELECT 1 FROM events o WHERE o.app = e.app AND o.env = 'production' AND o.name = 'error'
					AND o.day < ? AND json_extract(o.params, '$.id') = json_extract(e.params, '$.id'))
			GROUP BY id ORDER BY 2 DESC`, a.Key, day, day)
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var id, msg sql.NullString
			var n, users int
			if err := rs.Scan(&id, &n, &users, &msg); err != nil {
				rs.Close()
				return nil, err
			}
			t.Rows = append(t.Rows, []Cell{{Value: a.Name}, {Value: joinNonEmpty(" · ", id.String, msg.String)},
				{Value: fmt.Sprint(n)}, {Value: fmt.Sprint(users)}})
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return nil, err
		}
	}
	if len(t.Rows) == 0 {
		return nil, nil
	}
	return t, nil
}

// ---------- result helpers ----------

func rows(r *metrics.Result, table string) []map[string]any {
	for _, t := range r.Tables {
		if t.Name != table {
			continue
		}
		out := make([]map[string]any, 0, len(t.Rows))
		for _, row := range t.Rows {
			m := make(map[string]any, len(t.Columns))
			for i, c := range t.Columns {
				if i < len(row) {
					m[c] = row[i]
				}
			}
			out = append(out, m)
		}
		return out
	}
	return nil
}

func findRow(r *metrics.Result, table, col string, value any) map[string]any {
	for _, row := range rows(r, table) {
		if row[col] == value {
			return row
		}
	}
	return nil
}

// kpis turns the overview kpis table into key → [current, previous].
func kpis(r *metrics.Result) map[string][2]any {
	out := map[string][2]any{}
	for _, row := range rows(r, "kpis") {
		out[str(row["key"])] = [2]any{row["current"], row["previous"]}
	}
	return out
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

func toInt(v any) int { return int(math.Round(toFloat(v))) }

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// ---------- formatting ----------

func countCell(cur, prev int) Cell {
	c := Cell{Value: fmt.Sprint(cur)}
	if d := cur - prev; d != 0 {
		c.Delta, c.Up, c.Down = signed(fmt.Sprint(abs(d)), d > 0), d > 0, d < 0
	}
	return c
}

func decimalCell(cur, prev float64) Cell {
	c := Cell{Value: fmt.Sprintf("%.1f", cur)}
	if d := math.Round((cur-prev)*10) / 10; d != 0 {
		c.Delta, c.Up, c.Down = signed(fmt.Sprintf("%.1f", math.Abs(d)), d > 0), d > 0, d < 0
	}
	return c
}

func moneyCell(cur, prev float64) Cell {
	c := Cell{Value: money(cur)}
	if d := math.Round((cur-prev)*100) / 100; d != 0 {
		c.Delta, c.Up, c.Down = signed(money(math.Abs(d)), d > 0), d > 0, d < 0
	}
	return c
}

func pctCell(cur, prev float64) Cell {
	c := Cell{Value: fmt.Sprintf("%.1f%%", cur)}
	if d := math.Round((cur-prev)*10) / 10; d != 0 {
		c.Delta, c.Up, c.Down = signed(fmt.Sprintf("%.1f pt", math.Abs(d)), d > 0), d > 0, d < 0
	}
	return c
}

func signed(s string, up bool) string {
	if up {
		return "+" + s
	}
	return "-" + s
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// money formats an amount in the base currency; whole amounts drop the decimals.
func money(v float64) string {
	v = math.Round(v*100) / 100
	s := i18n.Money(v, currency)
	if v == math.Trunc(v) {
		s = strings.Replace(s, ".00", "", 1)
	}
	return s
}

// retained formats a retention percentage as "x% (n users)"; samples are small, so the head count is shown too.
func retained(pct any, size int) string {
	if pct == nil {
		return "—"
	}
	p := toFloat(pct)
	return fmt.Sprintf("%.0f%%", p) + paren(people(int(math.Round(p*float64(size)/100))))
}

// units formats a count with its unit: English picks one or many, Chinese uses zh.
func units(n int, one, many, zh string) string {
	if n == 1 {
		return i18n.T(fmt.Sprintf("%d %s", n, one), fmt.Sprintf("%d %s", n, zh))
	}
	return i18n.T(fmt.Sprintf("%d %s", n, many), fmt.Sprintf("%d %s", n, zh))
}

// plural is units, or empty for zero.
func plural(n int, one, many, zh string) string {
	if n == 0 {
		return ""
	}
	return units(n, one, many, zh)
}

// people formats a user count, e.g. "3 users".
func people(n int) string { return units(n, "user", "users", "人") }

// paren wraps s in parentheses: " (s)" in English, full-width "（s）" in Chinese.
func paren(s string) string { return i18n.T(" ("+s+")", "（"+s+"）") }

func errorColumns() []string {
	return []string{"App", i18n.T("Error", "错误"), i18n.T("Count", "次数"), i18n.T("Users affected", "影响人数")}
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

var weekdays = []i18n.Pair{{"Sun", "周日"}, {"Mon", "周一"}, {"Tue", "周二"}, {"Wed", "周三"}, {"Thu", "周四"}, {"Fri", "周五"}, {"Sat", "周六"}}

func parseDay(s string) time.Time {
	t, _ := tz.ParseDay(s)
	return t
}

func addDays(day string, n int) string { return parseDay(day).AddDate(0, 0, n).Format("2006-01-02") }

// dayDiff rounds because a day is 23 or 25 hours long across daylight-saving changes.
func dayDiff(from, to string) int {
	return int(math.Round(parseDay(to).Sub(parseDay(from)).Hours() / 24))
}

// dayLabel formats a day as "Sep 29" (Chinese "9/29"), or when long as "Tuesday, September 29, 2026"
// (Chinese year-month-day plus weekday).
func dayLabel(day string, long bool) string {
	t := parseDay(day)
	if long {
		return i18n.T(t.Format("Monday, January 2, 2006"),
			fmt.Sprintf("%d 年 %d 月 %d 日 %s", t.Year(), t.Month(), t.Day(), weekdays[t.Weekday()][1]))
	}
	return i18n.T(t.Format("Jan 2"), fmt.Sprintf("%d/%d", t.Month(), t.Day()))
}

// rangeLabel formats a day range as "Sep 22–28" (or "Sep 29–Oct 5") / "9/22–9/28".
func rangeLabel(from, to string) string {
	if i18n.Current() != i18n.ZH {
		f, t := parseDay(from), parseDay(to)
		if f.Month() == t.Month() && f.Year() == t.Year() {
			return f.Format("Jan 2") + "–" + t.Format("2")
		}
	}
	return dayLabel(from, false) + "–" + dayLabel(to, false)
}
