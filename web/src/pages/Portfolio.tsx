// Portfolio: revenue and scale across all apps; money first, monthly, side by side
import { useState } from 'react'
import clsx from 'clsx'
import { useNavigate, useSearchParams } from 'react-router'
import { useQuery, keepPreviousData } from '@tanstack/react-query'
import { Calendar, CloudDownload } from 'lucide-react'
import { api, num, rows, type Cell } from '../api'
import { paletteSlots, productName, slotColor, useMeta } from '../data'
import { addDays, countryName, fmtInt, fmtLongDay, fmtMoney, fmtMoneyAxis, fmtMonth, fmtPct, delta, moneyBasis } from '../format'
import { baseCurrency, t } from '../prefs'
import { DateRangePopover, Page } from '../components/Layout'
import {
  Badge, BarList, Card, DataTable, DeltaBadge, Dot, EmptyState, Grid, InlineBar, KpiCard, KpiRow, QueryState, Segmented,
  type Column, type Tone,
} from '../components/ui'
import { DonutChart, TrendChart } from '../components/Chart'

type Row = Record<string, Cell>

const kindMeta = (): Record<string, { label: string; color: string }> => ({
  iap: { label: t('买断', 'One-time'), color: 'var(--c1)' },
  subscription_new: { label: t('新订阅', 'New subs'), color: 'var(--c2)' },
  // Apple's sales reports count the first charge after a free trial as a renewal
  subscription_renewal: { label: t('续订（含试用转付费）', 'Renewals (incl. trial conversions)'), color: 'var(--c5)' },
  paid_download: { label: t('付费下载', 'Paid downloads'), color: 'var(--c3)' },
  other: { label: t('其他', 'Other'), color: 'var(--text-3)' },
})

// App Store notification type -> live feed badge
function eventBadge(type: string, subtype: string): { label: string; tone: Tone } {
  switch (type) {
    case 'ONE_TIME_CHARGE': return { label: t('买断', 'Purchase'), tone: 'success' }
    case 'SUBSCRIBED': return { label: subtype === 'RESUBSCRIBE' ? t('重新订阅', 'Resubscribed') : t('新订阅', 'New sub'), tone: 'success' }
    case 'DID_RENEW': return { label: subtype === 'TRIAL_CONVERSION' ? t('试用转付费', 'Trial converted') : t('续订', 'Renewed'), tone: 'success' }
    case 'REFUND': return { label: t('退款', 'Refund'), tone: 'danger' }
    case 'REFUND_REVERSED': return { label: t('退款撤销', 'Refund reversed'), tone: 'success' }
    case 'DID_CHANGE_RENEWAL_STATUS':
      return subtype === 'AUTO_RENEW_ENABLED' ? { label: t('开启自动续订', 'Auto-renew on'), tone: 'neutral' } : { label: t('关闭自动续订', 'Auto-renew off'), tone: 'warning' }
    case 'DID_FAIL_TO_RENEW': return { label: t('续订失败', 'Renewal failed'), tone: 'warning' }
    case 'EXPIRED': return { label: t('订阅过期', 'Expired'), tone: 'warning' }
    case 'GRACE_PERIOD_EXPIRED': return { label: t('宽限期结束', 'Grace period ended'), tone: 'warning' }
    case 'CONSUMPTION_REQUEST': return { label: t('退款申请', 'Refund request'), tone: 'warning' }
  }
  return { label: type, tone: 'neutral' }
}

const monthAxis = (m: string) => fmtMonth(Number(m.slice(5)))
const fmtMonthLong = (m: string) => fmtLongDay(m.slice(0, 7))

export function PortfolioPage() {
  const meta = useMeta()
  const navigate = useNavigate()
  const [sp, setSp] = useSearchParams()
  // Range: today (default, real-time view), or for the trend, revenue mix and countries week / month / year (last 7 days / 30 days / 12 months) or custom dates
  const customFrom = sp.get('from') ?? ''
  const customTo = sp.get('to') ?? ''
  const isCustom = /^\d{4}-\d{2}-\d{2}$/.test(customFrom) && /^\d{4}-\d{2}-\d{2}$/.test(customTo)
  const range = isCustom ? 'custom' : ['week', 'month', 'year'].includes(sp.get('range') ?? '') ? sp.get('range')! : 'today'
  const isToday = range === 'today'
  const [picking, setPicking] = useState(false)
  const setRange = (patch: Record<string, string | null>) =>
    setSp((prev) => {
      const n = new URLSearchParams(prev)
      for (const [k, v] of Object.entries(patch)) if (v === null) n.delete(k); else n.set(k, v)
      return n
    })
  const q = useQuery({
    queryKey: ['portfolio', range, customFrom, customTo],
    queryFn: () => api.metric('portfolio', range === 'custom' ? { range, from: customFrom, to: customTo } : { range: range === 'year' ? undefined : range }),
    placeholderData: keepPreviousData,
    enabled: !isToday,
  })
  // Today: real-time sources only, refreshed every minute
  const tq = useQuery({
    queryKey: ['today'],
    queryFn: () => api.metric('today', {}),
    enabled: isToday,
    refetchInterval: 60_000,
  })
  const todayPeriod = rows(tq.data, 'period')[0]
  const todayApps = rows<Row>(tq.data, 'apps')
  const todayFeed = rows<Row>(tq.data, 'feed')

  const status = rows(q.data, 'status')[0]
  const synced = status?.synced === true
  const kpi = new Map(rows<{ key: string; current: Cell; previous: Cell }>(q.data, 'kpis').map((r) => [r.key, r]))
  const k = (key: string) => kpi.get(key) ?? { current: null, previous: null }
  const period = rows(q.data, 'period')[0]
  const apps = rows<Row>(q.data, 'apps')
  const trend = rows<Row>(q.data, 'trend')
  const byDay = period?.granularity === 'day'
  const reportDay = String(period?.through_day ?? meta.today)
  const daily = rows<Row>(q.data, 'daily')
  const feed = rows<Row>(q.data, 'feed')
  // Store conversion: analytics reports, fixed to the last 28 days ending on the latest report day
  const sq = useQuery({
    queryKey: ['store_funnel', 'all', meta.today],
    queryFn: () => api.metric('store_funnel', { scope: 'all', from: addDays(meta.today, -27), to: meta.today }),
    placeholderData: keepPreviousData,
  })
  const storeSynced = rows(sq.data, 'status')[0]?.synced === true
  const storeWin = rows(sq.data, 'window')[0]
  const storeApps = rows<Row>(sq.data, 'apps')

  // App colors and names: palette slots from meta; bundles not in meta take the remaining slots; names prefer the dashboard app name
  const bundles = [...new Set([...apps.map((a) => String(a.bundle_id)), ...meta.apps.map((a) => a.bundle), ...feed.map((f) => String(f.bundle_id)),
    ...todayApps.map((a) => String(a.bundle_id))])]
    .filter(Boolean)
    .sort((a, b) => a.localeCompare(b))
  const appOf = (b: string) => meta.apps.find((a) => a.bundle === b)
  const freeSlots = paletteSlots.filter((s) => !meta.apps.some((a) => a.color === s))
  const others = bundles.filter((b) => !appOf(b))
  const colorOf = (b: string) => {
    const a = appOf(b)
    if (a) return slotColor(a.color)
    const pool = freeSlots.length ? freeSlots : paletteSlots
    return slotColor(pool[Math.max(0, others.indexOf(b)) % pool.length])
  }
  const nameOf = (b: string) => appOf(b)?.name ?? String(apps.find((a) => a.bundle_id === b)?.asc_name ?? b)
  const keyOf = (b: string) => appOf(b)?.key
  const productOf = (b: string, id: string) => productName(appOf(b) ?? { bundle: b }, id)

  // By day: fill days without sales (up to the latest report day); by month: keep the months that have data
  const trendAxis: string[] = []
  if (byDay) {
    const end = String(period.range_to) < reportDay ? String(period.range_to) : reportDay
    for (let d = String(period.range_from); d <= end; d = addDays(d, 1)) trendAxis.push(d)
  } else {
    trendAxis.push(...new Set(trend.map((m) => String(m.bucket))))
    trendAxis.sort()
  }
  const appsInChart = bundles.filter((b) => trend.some((m) => m.bundle_id === b))
  const trendSeries = (field: 'proceeds' | 'downloads') =>
    appsInChart.map((b) => ({
      name: nameOf(b), type: 'bar' as const, stack: 'm', color: colorOf(b),
      data: trendAxis.map((m) => num(trend.find((r) => r.bucket === m && r.bundle_id === b)?.[field])),
    }))
  const trendX = byDay ? {} : { xFormat: { axis: monthAxis, tooltip: fmtMonthLong } }
  const rangeLabel = range === 'week' ? t('近 7 天', 'Last 7 days')
    : range === 'month' ? t('近 30 天', 'Last 30 days')
    : range === 'year' ? t('近 12 个月', 'Last 12 months')
    : `${customFrom} ~ ${customTo}`

  const structure = rows<Row>(q.data, 'structure').filter((s) => num(s.proceeds) !== 0)
  const countries = rows<Row>(q.data, 'countries')
  const max12 = Math.max(...apps.map((a) => num(a.last_12m_proceeds)), 1)

  const subtitle = isToday
    ? t(`全部 App 今天的实时数据${todayPeriod ? ` · 截至 ${todayPeriod.now_local}，对比昨日同一时刻` : ''}`,
      `Today across all apps, real time${todayPeriod ? ` · as of ${todayPeriod.now_local}, vs. yesterday at the same time` : ''}`)
    : synced && period
    ? t(`全部 App 的收入与规模 · 报表截至 ${fmtLongDay(String(period.through_day))}（太平洋时间）`,
      `Revenue and scale across all apps · reports through ${fmtLongDay(String(period.through_day))} (Pacific Time)`)
    : t('全部 App 的收入与规模', 'Revenue and scale across all apps')
  const kinds = kindMeta()

  const maxCvr = Math.max(...storeApps.map((a) => num(a.cvr)), 0.1)
  const storeColumns: Column<Row>[] = [
    {
      key: 'app', title: 'App', render: (r) => (
        <span className="flex items-center gap-2">
          <Dot color={colorOf(String(r.bundle_id))} />
          <span className="font-medium">{nameOf(String(r.bundle_id))}</span>
        </span>
      ),
    },
    { key: 'imp', title: t('曝光', 'Impressions'), align: 'right', sortValue: (r) => num(r.impressions_unique), render: (r) => fmtInt(r.impressions_unique) },
    { key: 'pv', title: t('产品页', 'Page views'), align: 'right', sortValue: (r) => num(r.page_views_unique), render: (r) => fmtInt(r.page_views_unique) },
    { key: 'first', title: t('首次下载', 'First downloads'), align: 'right', sortValue: (r) => num(r.first_downloads), render: (r) => fmtInt(r.first_downloads) },
    {
      // Bar and conversion share one cell: length is normalized to the highest conversion among apps, so it is not misread as page conversion
      key: 'cvr', title: t('转化率', 'Conversion'), align: 'right', width: '260px', sortValue: (r) => num(r.cvr), render: (r) => (
        <span className="flex items-center gap-3">
          <InlineBar value={num(r.cvr)} max={maxCvr} color={colorOf(String(r.bundle_id))} />
          <span className="w-12 shrink-0 text-right font-medium">{fmtPct(r.cvr)}</span>
          <DeltaBadge value={storeWin?.prev_complete === true ? delta(r.cvr, r.prev_cvr) : null} />
        </span>
      ),
    },
    { key: 'page_cvr', title: t('产品页转化', 'Page conversion'), align: 'right', sortValue: (r) => num(r.page_cvr), render: (r) => <span className="text-text-2">{fmtPct(r.page_cvr)}</span> },
  ]

  const columns: Column<Row>[] = [
    {
      key: 'app', title: 'App', render: (r) => (
        <span className="flex items-center gap-2">
          <Dot color={colorOf(String(r.bundle_id))} />
          <span className="font-medium">{nameOf(String(r.bundle_id))}</span>
        </span>
      ),
    },
    {
      key: 'mtd', title: t('本月至今', 'Month to date'), align: 'right', sortValue: (r) => num(r.mtd_proceeds), render: (r) => (
        <span className="inline-flex items-center gap-2">
          {fmtMoney(r.mtd_proceeds)}
          <DeltaBadge value={delta(r.mtd_proceeds, r.prev_same_proceeds)} />
        </span>
      ),
    },
    { key: 'last', title: t('上月', 'Last month'), align: 'right', sortValue: (r) => num(r.last_month_proceeds), render: (r) => <span className="text-text-2">{fmtMoney(r.last_month_proceeds)}</span> },
    { key: '12m', title: t('近 12 个月', 'Last 12 months'), align: 'right', sortValue: (r) => num(r.last_12m_proceeds), render: (r) => fmtMoney(r.last_12m_proceeds) },
    {
      key: 'share', title: t('占比', 'Share'), width: '160px', render: (r) => (
        <span className="flex items-center gap-2">
          <InlineBar value={num(r.last_12m_proceeds)} max={max12} color={colorOf(String(r.bundle_id))} />
          <span className="num w-12 shrink-0 text-right text-text-2">{fmtPct(r.share_12m_pct)}</span>
        </span>
      ),
    },
    {
      key: 'dl', title: t('本月下载', 'Downloads MTD'), align: 'right', sortValue: (r) => num(r.mtd_downloads), render: (r) => (
        <span className="inline-flex items-center gap-2">
          {fmtInt(r.mtd_downloads)}
          <DeltaBadge value={delta(r.mtd_downloads, r.prev_same_downloads)} />
        </span>
      ),
    },
    { key: 'mau', title: t('月活', 'MAU'), align: 'right', render: (r) => (r.analytics ? fmtInt(r.mau_30d) : <span className="text-text-3">—</span>) },
    { key: 'new', title: t('30 天新增', 'New (30d)'), align: 'right', render: (r) => (r.analytics ? fmtInt(r.new_users_30d) : <span className="text-text-3">—</span>) },
    { key: 'sdk', title: t('埋点', 'Analytics'), render: (r) => (r.analytics ? <Badge tone="success">{t('已接入', 'Enabled')}</Badge> : <Badge>{t('未接入', 'Not set up')}</Badge>) },
  ]

  return (
    <Page
      title={t('经营总览', 'Portfolio')}
      subtitle={subtitle}
      actions={
        <div className="relative flex shrink-0 items-center gap-1">
          <Segmented
            value={range}
            ariaLabel={t('时间范围', 'Date range')}
            onChange={(v) => v !== 'custom' && setRange({ range: v === 'today' ? null : v, from: null, to: null })}
            options={[
              { value: 'today', label: t('今日', 'Today') },
              { value: 'week', label: t('周', 'Week') }, { value: 'month', label: t('月', 'Month') }, { value: 'year', label: t('年', 'Year') },
              ...(range === 'custom' ? [{ value: 'custom', label: `${customFrom.slice(5)} ~ ${customTo.slice(5)}` }] : []),
            ]}
          />
          {!isToday && <button
            type="button"
            aria-label={t('自定义日期', 'Custom dates')}
            onClick={() => setPicking((v) => !v)}
            className={clsx('press flex size-8 items-center justify-center rounded-full hover:bg-fill', picking || range === 'custom' ? 'text-primary' : 'text-text-2')}
          >
            <Calendar size={16} strokeWidth={1.75} />
          </button>}
          {picking && !isToday && (
            <DateRangePopover
              from={isCustom ? customFrom : byDay ? String(period.range_from) : addDays(reportDay, -29)}
              to={isCustom ? customTo : reportDay}
              max={reportDay}
              onApply={(from, to) => setRange({ from, to, range: null })}
              onClose={() => setPicking(false)}
            />
          )}
        </div>
      }
    >
      {isToday ? (
        tq.isPending ? (
          <div className="skeleton h-[420px] w-full" />
        ) : tq.error ? (
          <QueryState loading={false} error={tq.error} onRetry={() => tq.refetch()}>{null}</QueryState>
        ) : (
          <TodayView data={tq.data} apps={todayApps} feed={todayFeed} colorOf={colorOf} nameOf={nameOf} keyOf={keyOf} productOf={productOf} />
        )
      ) : q.isPending ? (
        <div className="skeleton h-[420px] w-full" />
      ) : q.error ? (
        <QueryState loading={false} error={q.error} onRetry={() => q.refetch()}>{null}</QueryState>
      ) : !synced ? (
        <Card>
          <div className="flex flex-col items-center gap-2 py-10 text-center">
            <CloudDownload size={28} strokeWidth={1.5} className="text-text-3" />
            <div className="text-body text-text-2">{t('还没有同步 App Store Connect 销售报表', 'App Store Connect sales reports not synced yet')}</div>
            <div className="max-w-md text-small text-text-3">
              {t('服务器配置 ASC_KEY_ID、ASC_ISSUER_ID、ASC_KEY_PATH、ASC_VENDOR_NUMBER 后会自动回填历史收入，每 6 小时同步一次。',
                'Once the server sets ASC_KEY_ID, ASC_ISSUER_ID, ASC_KEY_PATH and ASC_VENDOR_NUMBER, historical revenue is backfilled automatically and synced every 6 hours.')}
            </div>
          </div>
        </Card>
      ) : (
        <>
          <KpiRow>
            <KpiCard label={t('本月至今到手', 'Proceeds MTD')} value={fmtMoney(k('mtd_proceeds').current)} current={k('mtd_proceeds').current} previous={k('mtd_proceeds').previous}
              spark={daily.map((d) => num(d.proceeds))} sparkColor="var(--money)"
              info={t(`${period?.this_month} 月 1 日至 ${String(period?.through_day).slice(5)} 的到手收入（已扣佣金与税，${moneyBasis()}），对比上月同期`,
                `Proceeds from ${fmtMonth(Number(period?.this_month))} 1 to ${String(period?.through_day).slice(5)} (net of commission and tax, ${moneyBasis()}), vs. same period last month`)} />
            <KpiCard label={t('上月到手', 'Proceeds last month')} value={fmtMoney(k('last_month_proceeds').current)} info={t(`${fmtMonthLong(String(period?.last_month))}全月`, `All of ${fmtMonthLong(String(period?.last_month))}`)} />
            <KpiCard label={t('今年累计', 'Year to date')} value={fmtMoney(k('ytd_proceeds').current)} info={t('今年 1 月至今的到手收入', 'Proceeds since January this year')} />
            <KpiCard label={t('近 12 个月', 'Last 12 months')} value={fmtMoney(k('last_12m_proceeds').current)} info={t(`含本月；有记录以来累计 ${fmtMoney(k('all_time_proceeds').current)}`, `Includes this month; all-time total ${fmtMoney(k('all_time_proceeds').current)}`)} />
            <KpiCard label={t('本月下载', 'Downloads MTD')} value={fmtInt(k('mtd_downloads').current)} current={k('mtd_downloads').current} previous={k('mtd_downloads').previous}
              spark={daily.map((d) => num(d.downloads))} sparkColor="var(--c5)" info={t('首次下载（不含重新下载、更新）', 'First-time downloads (excludes redownloads and updates)')} />
            <KpiCard label={t('本月付费笔数', 'Paid units MTD')} value={fmtInt(k('mtd_paid_units').current)} current={k('mtd_paid_units').current} previous={k('mtd_paid_units').previous}
              info={t('买断、订阅（含续订）与付费下载的笔数', 'One-time purchases, subscriptions (incl. renewals) and paid downloads')} />
            <KpiCard label={t('本月退款', 'Refunds MTD')} value={fmtInt(k('mtd_refunds').current)} suffix={t('笔', '')} current={k('mtd_refunds').current} previous={k('mtd_refunds').previous} inverse
              info={t('销售报表中数量为负的付费行', 'Paid rows with negative units in sales reports')} />
            <KpiCard label={t('报表后的实时成交', 'Live sales after report')} value={fmtMoney(k('live_after_report').current)} suffix={t(`${fmtInt(k('live_after_report').previous)} 笔`, `${fmtInt(k('live_after_report').previous)} txns`)}
              info={t('报表还没覆盖的最近成交（App Store 通知），金额为售价、未扣佣金；笔数不含免费试用开通', 'Recent sales not yet in reports (App Store notifications); amounts are list price before commission; excludes free-trial starts')} />
          </KpiRow>

          <Card title={byDay ? t('每日到手收入', 'Daily proceeds') : t('月度到手收入', 'Monthly proceeds')}
            info={range === 'year'
              ? t(`${rangeLabel}，按 App 堆叠；有月报的月份用月报，其余由日报累加；外币按同步时汇率折合为 ${baseCurrency()}（展示：${moneyBasis()}）`, `${rangeLabel}, stacked by app; monthly reports where available, otherwise summed daily reports; foreign currency converted to ${baseCurrency()} at sync-time rates (shown ${moneyBasis()})`)
              : t(`${rangeLabel}，按 App 堆叠；来自日报，外币按同步时汇率折合为 ${baseCurrency()}（展示：${moneyBasis()}）`, `${rangeLabel}, stacked by app; from daily reports, foreign currency converted to ${baseCurrency()} at sync-time rates (shown ${moneyBasis()})`)}>
            {trendAxis.length ? (
              <TrendChart height={300} x={trendAxis} series={trendSeries('proceeds')} format={(v) => fmtMoney(v)} axisFormat={fmtMoneyAxis} showTotal {...trendX} />
            ) : <EmptyState />}
          </Card>

          <Card title={t('App 对比', 'App comparison')} info={t('本月至今与上月同期比较；月活、新增来自自建埋点（近 30 天，正式环境）', 'Month to date vs. same period last month; MAU and new users come from in-house analytics (last 30 days, production)')}>
            <DataTable
              columns={columns}
              data={apps}
              rowKey={(r) => String(r.bundle_id)}
              defaultSort={{ key: '12m', desc: true }}
              onRowClick={(r) => {
                const key = keyOf(String(r.bundle_id))
                if (key && r.analytics) navigate(`/overview?app=${key}`)
              }}
            />
          </Card>

          <Grid>
            <Card span={5} title={t('收入结构', 'Revenue mix')} info={t(`${rangeLabel}的到手收入按类型拆分`, `Proceeds by type, ${rangeLabel}`)}>
              {structure.length ? (
                <DonutChart
                  centerLabel={rangeLabel}
                  format={(v) => fmtMoney(v)}
                  items={structure.map((s) => ({ name: kinds[String(s.kind)]?.label ?? String(s.kind), value: num(s.proceeds), color: kinds[String(s.kind)]?.color ?? 'var(--text-3)' }))}
                />
              ) : <EmptyState hint={t('这段时间没有付费收入', 'No paid revenue in this period')} />}
              <ul className="mt-2 grid grid-cols-2 gap-x-4 gap-y-1.5">
                {structure.map((s) => (
                  <li key={String(s.kind)} className="flex items-center gap-2 text-small">
                    <Dot color={kinds[String(s.kind)]?.color ?? 'var(--text-3)'} />
                    <span className="text-text-2">{kinds[String(s.kind)]?.label ?? String(s.kind)}</span>
                    <span className="num ml-auto font-medium text-text">{fmtMoney(s.proceeds)}</span>
                  </li>
                ))}
              </ul>
            </Card>
            <Card span={7} title={t('国家 / 地区', 'Countries / regions')} info={t(`${rangeLabel}，按到手收入排序`, `${rangeLabel}, sorted by proceeds`)}>
              <BarList
                valueLabel={t('到手收入', 'Proceeds')}
                secondary={t('下载', 'Downloads')}
                format={(v) => fmtMoney(v)}
                items={countries.map((c) => ({ key: String(c.country), label: countryName(String(c.country)), value: num(c.proceeds), sub: fmtInt(c.downloads), color: 'var(--money)' }))}
              />
            </Card>
            <Card span={7} title={byDay ? t('每日下载', 'Daily downloads') : t('月度下载', 'Monthly downloads')} info={t(`${rangeLabel}，首次下载，按 App 堆叠`, `${rangeLabel}, first-time downloads, stacked by app`)}>
              {trendAxis.length ? (
                <TrendChart height={280} x={trendAxis} series={trendSeries('downloads')} format={fmtInt} showTotal {...trendX} />
              ) : <EmptyState />}
            </Card>
            <Card span={5} title={t('实时动态', 'Live feed')} info={t('App Store 服务器通知（正式环境），金额为售价', 'App Store server notifications (production); amounts are list price')}>
              <Feed feed={feed} colorOf={colorOf} nameOf={nameOf} productOf={productOf} />
            </Card>
          </Grid>

          {storeSynced && storeApps.length > 0 && (
            <Card title={t('商店转化', 'Store conversion')}
              info={t(`近 28 天（${storeWin?.from} 至 ${storeWin?.to}，App Store Connect 分析报表）。曝光、产品页为去重设备；转化率 = 首次下载 ÷ 去重曝光，与 App Store Connect 同口径`,
                `Last 28 days (${storeWin?.from} to ${storeWin?.to}, App Store Connect analytics reports). Impressions and page views are unique devices; conversion = first downloads ÷ unique impressions, same as App Store Connect`)}>
              <DataTable
                columns={storeColumns}
                data={storeApps}
                rowKey={(r) => String(r.bundle_id)}
                defaultSort={{ key: 'imp', desc: true }}
                onRowClick={(r) => {
                  const key = keyOf(String(r.bundle_id))
                  if (key) navigate(`/acquisition?app=${key}`)
                }}
              />
            </Card>
          )}
        </>
      )}

      {!isToday && !q.isPending && !synced && feed.length > 0 && (
        <Card title={t('实时动态', 'Live feed')} info={t('App Store 服务器通知（正式环境），金额为售价', 'App Store server notifications (production); amounts are list price')}>
          <Feed feed={feed} colorOf={colorOf} nameOf={nameOf} productOf={productOf} />
        </Card>
      )}
    </Page>
  )
}

function Feed({ feed, colorOf, nameOf, productOf }: {
  feed: Row[]
  colorOf: (b: string) => string
  nameOf: (b: string) => string
  productOf: (b: string, id: string) => string
}) {
  if (!feed.length) return <EmptyState title={t('暂无动态', 'No activity yet')} />
  return (
    <ul className="scroll-thin -mr-2 max-h-[300px] overflow-y-auto pr-2">
      {feed.map((f, i) => {
        // A zero-price start is a free trial: own badge, no amount
        const trial = num(f.revenue_sign) === 1 && (f.free === true || num(f.free) === 1)
        const b = trial ? { label: t('试用开通', 'Trial started'), tone: 'neutral' as Tone } : eventBadge(String(f.type), String(f.subtype ?? ''))
        const sign = trial ? 0 : num(f.revenue_sign)
        return (
          <li key={i} className="flex items-center gap-2.5 border-b border-border py-2.5 text-small last:border-b-0">
            <span className="num w-[76px] shrink-0 text-caption font-normal text-text-3">{String(f.time_local).slice(5, 16)}</span>
            <Dot color={colorOf(String(f.bundle_id))} />
            <span className="w-16 shrink-0 truncate text-text-2">{nameOf(String(f.bundle_id))}</span>
            <Badge tone={b.tone}>{b.label}</Badge>
            <span className="min-w-0 flex-1 truncate text-text-3" title={String(f.product_id ?? '')}>{productOf(String(f.bundle_id), String(f.product_id ?? ''))}</span>
            {sign !== 0 && (
              <span className={`num shrink-0 font-medium ${sign < 0 ? 'text-danger' : 'text-text'}`}>
                {sign < 0 ? '−' : ''}{fmtMoney(f.amount)}
              </span>
            )}
          </li>
        )
      })}
    </ul>
  )
}

function TodayView({ data, apps, feed, colorOf, nameOf, keyOf, productOf }: {
  data: Parameters<typeof rows>[0]
  apps: Row[]
  feed: Row[]
  colorOf: (b: string) => string
  nameOf: (b: string) => string
  keyOf: (b: string) => string | undefined
  productOf: (b: string, id: string) => string
}) {
  const navigate = useNavigate()
  const period = rows(data, 'period')[0]
  const kpi = new Map(rows<{ key: string; current: Cell; previous: Cell }>(data, 'kpis').map((r) => [r.key, r]))
  const k = (key: string) => kpi.get(key) ?? { current: null, previous: null }
  const hourly = rows<Row>(data, 'hourly')
  const asOf = String(period?.now_local ?? '')
  const vsYesterday = t(`对比昨日 0 点至 ${asOf}`, `vs. yesterday 00:00 to ${asOf}`)

  const hours = Array.from({ length: 24 }, (_, h) => h)
  const sumHour = (h: number, field: 'today' | 'yesterday') => {
    const r = hourly.filter((x) => num(x.hour) === h)
    if (field === 'today' && r.every((x) => x.today == null)) return null
    return r.reduce((s, x) => s + num(x[field]), 0)
  }
  const hourLabel = (v: string) => `${v.padStart(2, '0')}:00`

  const columns: Column<Row>[] = [
    {
      key: 'app', title: 'App', render: (r) => (
        <span className="flex items-center gap-2">
          <Dot color={colorOf(String(r.bundle_id))} />
          <span className="font-medium">{nameOf(String(r.bundle_id))}</span>
        </span>
      ),
    },
    ...(['active_users', 'new_users', 'sessions'] as const).map((f): Column<Row> => ({
      key: f, align: 'right', sortValue: (r) => num(r[f]),
      title: f === 'active_users' ? t('活跃用户', 'Active users') : f === 'new_users' ? t('真新增', 'New users') : t('会话', 'Sessions'),
      render: (r) => (r.analytics ? (
        <span className="inline-flex items-center gap-2">
          {fmtInt(r[f])}
          <DeltaBadge value={delta(r[f], r[`prev_${f}`])} />
        </span>
      ) : <span className="text-text-3">—</span>),
    })),
    {
      key: 'paywall', title: t('付费墙 → 成交', 'Paywall → Sales'), align: 'right', sortValue: (r) => num(r.paywall_users),
      render: (r) => <span className="text-text-2">{r.analytics ? fmtInt(r.paywall_users) : '—'} → {fmtInt(r.sales)}</span>,
    },
    {
      key: 'gross', title: t('流水', 'Gross'), align: 'right', sortValue: (r) => num(r.gross), render: (r) => (
        <span className="inline-flex items-center gap-2">
          {fmtMoney(r.gross)}
          <DeltaBadge value={delta(r.gross, r.prev_gross)} />
        </span>
      ),
    },
    {
      key: 'errors', title: t('错误', 'Errors'), align: 'right', sortValue: (r) => num(r.errors),
      render: (r) => (r.analytics ? <span className={num(r.errors) > 0 ? 'text-danger' : 'text-text-3'}>{fmtInt(r.errors)}</span> : <span className="text-text-3">—</span>),
    },
  ]

  return (
    <>
      <KpiRow>
        <KpiCard label={t('今日流水', 'Gross today')} value={fmtMoney(k('gross').current)} current={k('gross').current} previous={k('gross').previous}
          info={t(`App Store 实时通知的成交减退款，金额为售价、未扣佣金（${moneyBasis()}）；${vsYesterday}`,
            `Sales minus refunds from App Store notifications, list price before commission (${moneyBasis()}); ${vsYesterday}`)} />
        <KpiCard label={t('成交', 'Sales')} value={fmtInt(k('sales').current)} suffix={t('笔', '')} current={k('sales').current} previous={k('sales').previous}
          info={t(`售价大于 0 的付费成交（买断、新订阅、续订）；${vsYesterday}`, `Paid transactions with price above 0 (purchases, new subs, renewals); ${vsYesterday}`)} />
        <KpiCard label={t('试用开通', 'Trials started')} value={fmtInt(k('trials').current)} suffix={t('笔', '')} current={k('trials').current} previous={k('trials').previous}
          info={t(`售价为 0 的订阅开通，不算成交；${vsYesterday}`, `Subscription starts at price 0, not counted as sales; ${vsYesterday}`)} />
        <KpiCard label={t('退款', 'Refunds')} value={fmtInt(k('refunds').current)} suffix={t('笔', '')} current={k('refunds').current} previous={k('refunds').previous} inverse
          info={t(`App Store 退款通知；${vsYesterday}`, `App Store refund notifications; ${vsYesterday}`)} />
        <KpiCard label={t('活跃用户', 'Active users')} value={fmtInt(k('active_users').current)} current={k('active_users').current} previous={k('active_users').previous}
          info={t(`今天有任意事件的去重用户，各 App 相加（正式环境）；${vsYesterday}`, `Unique users with any event today, summed across apps (production); ${vsYesterday}`)} />
        <KpiCard label={t('真新增', 'New users')} value={fmtInt(k('new_users').current)} current={k('new_users').current} previous={k('new_users').previous}
          info={t(`首次下载日 = 首见日的新用户；${vsYesterday}`, `Users whose first download day equals first-seen day; ${vsYesterday}`)} />
        <KpiCard label={t('付费墙曝光', 'Paywall viewers')} value={fmtInt(k('paywall_users').current)} current={k('paywall_users').current} previous={k('paywall_users').previous}
          info={t(`看到付费墙（paywall.shown）的去重用户，各 App 相加；${vsYesterday}`, `Unique users who saw a paywall (paywall.shown), summed across apps; ${vsYesterday}`)} />
        <KpiCard label={t('错误', 'Errors')} value={fmtInt(k('errors').current)} suffix={t('次', '')} current={k('errors').current} previous={k('errors').previous} inverse
          info={t(`error 事件次数；${vsYesterday}`, `error event count; ${vsYesterday}`)} />
      </KpiRow>

      <Card title={t('各 App 今日', 'Apps today')} info={t(`变化为${vsYesterday}；成交为 App Store 确认的付费笔数`, `Changes are ${vsYesterday}; sales are paid transactions confirmed by the App Store`)}>
        <DataTable
          columns={columns}
          data={apps}
          rowKey={(r) => String(r.bundle_id)}
          defaultSort={{ key: 'active_users', desc: true }}
          onRowClick={(r) => {
            const key = keyOf(String(r.bundle_id))
            if (key && r.analytics) navigate(`/overview?app=${key}`)
          }}
        />
      </Card>

      <Grid>
        <Card span={7} title={t('每小时活跃', 'Active users by hour')}
          info={t('每小时有任意事件的去重用户，各 App 相加；昨日为全天，用来看今天剩下时段的走势', 'Unique users with any event in each hour, summed across apps; yesterday covers the whole day to show the shape of the rest of today')}>
          {hourly.length ? (
            <TrendChart
              height={280}
              x={hours.map(String)}
              xIsDay={false}
              xFormat={{ axis: hourLabel, tooltip: hourLabel }}
              format={fmtInt}
              series={[
                { name: t('今日', 'Today'), type: 'bar', color: 'var(--c1)', data: hours.map((h) => sumHour(h, 'today')) },
                { name: t('昨日', 'Yesterday'), type: 'line', color: 'var(--c2)', data: hours.map((h) => sumHour(h, 'yesterday')) },
              ]}
            />
          ) : <EmptyState />}
        </Card>
        <Card span={5} title={t('今日成交动态', 'Live feed today')} info={t('今天的 App Store 服务器通知（正式环境），金额为售价', "Today's App Store server notifications (production); amounts are list price")}>
          <Feed feed={feed} colorOf={colorOf} nameOf={nameOf} productOf={productOf} />
        </Card>
      </Grid>
    </>
  )
}
