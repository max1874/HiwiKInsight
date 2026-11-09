# Metrics

HiwiKInsight ships 15 built-in metrics with fixed definitions (`internal/metrics`). The same code serves the dashboard, the [query API](query-api.md), the [CLI](cli.md), MCP and the email reports, so a number means the same thing everywhere. The definition text below is exactly what the API returns in `definition` and what `describe` lists.

## Common parameters and conventions

| Parameter | Default | Meaning |
|---|---|---|
| `app` | first configured app | App key |
| `env` | `production` | `production`, `sandbox` or `xcode` |
| `from`, `to` | last 30 days ending today | Inclusive days (`YYYY-MM-DD`) in the reporting time zone; at most 400 days apart |

- **Day**: a calendar day in the configured `timezone`.
- **User**: an anonymous `install_id`. A reinstall is a new user.
- **Active**: had any event that day (a row in `daily_active`).
- **Truly new user**: an install whose first-seen day equals its first App Store download day (`installs.acquired_at`, from `AppTransaction`). People who installed before the app shipped the SDK are not new.
- **Money**: in the base currency. Revenue from App Store notifications is the customer price converted at the day's rate (before Apple's commission); `portfolio` uses App Store Connect proceeds (after commission and taxes). A transaction with price 0 is not a sale; it is a free-trial start only when it starts a subscription (`SUBSCRIBED`) without an offer code.
- Percentages have one decimal; `null` means no data or not yet observable.

```bash
insight metric <name> --app pawprint --env production --from 2026-09-01 --to 2026-09-30 [--param value]
GET /v1/query/metric/<name>?app=pawprint&env=production&from=2026-09-01&to=2026-09-30&param=value
```

## Index

| Metric | Answers |
|---|---|
| [`active_users`](#active_users) | DAU, WAU, MAU, stickiness, new users per day |
| [`activity_days`](#activity_days) | Do users come back every day? |
| [`retention`](#retention) | D1 to D30 retention of truly new users by cohort |
| [`returning`](#returning) | New, returning, resurrected; at-risk and churned users |
| [`module_usage`](#module_usage) | Which modules, screens and features are used |
| [`funnel`](#funnel) | Paywall funnel by entry point |
| [`revenue`](#revenue) | Real revenue by entry point, product, storefront, day; days to purchase |
| [`distribution`](#distribution) | Devices, OS and app versions, regions, storefronts, languages |
| [`overview`](#overview) | KPIs vs. previous period, daily trend, user tiers |
| [`sessions_daily`](#sessions_daily) | Sessions per day and their duration |
| [`users`](#users) | List of active users with tiers and filters |
| [`event_trend`](#event_trend) | Any event over time, optionally grouped by a param |
| [`errors`](#errors) | Errors by id |
| [`portfolio`](#portfolio) | Proceeds and downloads across all apps (App Store Connect) |
| [`today`](#today) | Today so far across all apps vs. yesterday at the same time (real time) |
| [`store_funnel`](#store_funnel) | App Store impressions to page views to downloads |

---

## active_users

> Daily DAU (distinct install_ids with any event that day), WAU (trailing 7 days including the day), MAU (trailing 30 days), stickiness DAU/MAU, and the number of truly new users that day (first download day = first seen day)

Extra parameters: none.

| Table | Columns |
|---|---|
| `summary` | `days`, `avg_dau`, `users_in_range` (distinct active users in the range), `new_users` (truly new users in the range) |
| `daily` | `day`, `dau`, `wau`, `mau`, `new_users`, `stickiness_pct` |

## activity_days

> As of the `to` day: distribution of how many days each user was active in the last 7 days (L7) and last 28 days (L28); answers "do users come back every day?"

Extra parameters: none; only `to` matters.

| Table | Columns |
|---|---|
| `l7` | `active_days_in_7` (1 to 7), `users`, `share_pct` (of users active in the last 7 days) |
| `l28` | `active_days_in_28` (buckets `1`, `2-3`, `4-7`, `8-14`, `15-27`, `28`), `users`, `share_pct` (of users active in the last 28 days) |

## retention

> Retention of truly new users grouped by first seen day (by the Monday of the first seen week when cohort=week): Dn = share active on exactly day n after the first seen day; within_7d / within_30d = share that came back at least once within n days; cells not yet observable are empty

Extra parameters: `cohort=week` groups cohorts by week (Monday); default is per day. `from`/`to` select the cohorts' first-seen days.

| Table | Columns |
|---|---|
| `cohorts` | `cohort_day`, `new_users`, `d1_pct`, `d3_pct`, `d7_pct`, `d14_pct`, `d30_pct`, `within_7d_pct`, `within_30d_pct`; a final row `all` aggregates every cohort |

A cell is `null` when day n has not arrived yet for anyone in the cohort; the denominators count only users whose day n has arrived.

## returning

> Daily active users split into new / returning (not first seen that day) / resurrected (inactive for >= 7 consecutive days before); plus, as of `to`, churned (last active >= 14 days ago) and at-risk (7-13 days) user counts

Extra parameters: none. "New" here means the user's first active day (not necessarily a truly new user).

| Table | Columns |
|---|---|
| `summary` | `users_ever`, `resurrected_users_in_range`, `at_risk_7_13d`, `churned_14d_plus` |
| `daily` | `day`, `active`, `new`, `returning`, `resurrected` (subset of returning) |

## module_usage

> screen.viewed by module / screen: coverage = users who used the module / active users in the range; views per user; average time on screen in seconds (from screen.left). The features table counts feature-usage events carrying a module parameter (e.g. AI analysis, OCR) by module / event / feature: users, uses and success rate

Extra parameters: none.

| Table | Columns |
|---|---|
| `summary` | `active_users` |
| `modules` | `module`, `users`, `coverage_pct`, `views`, `views_per_user`, `avg_duration_s` |
| `screens` | `module_screen` (`module/screen`), `users`, `coverage_pct`, `views`, `views_per_user`, `avg_duration_s` |
| `features` | `module`, `event`, `feature` (first of the `feature`, `source`, `action` params), `users`, `coverage_pct`, `uses`, `uses_per_user`, `success_pct` (share of events with `result = success` among events that have a `result`) |

## funnel

> Paywall funnel split by entry context: distinct users and conversion rates for paywall.shown -> paywall.cta_tap -> purchase.started -> purchase.success; verified_sales = sales confirmed by the App Store server (price > 0), verified_trials = free-trial starts (subscriptions started at price 0 without an offer code, not counted as sales)

Extra parameters: none. Events without a `context` param count as `unknown`.

| Table | Columns |
|---|---|
| `by_context` | `context`, `shown_users`, `cta_users`, `started_users`, `success_users`, `verified_sales`, `verified_trials`, `cta_rate_pct` (cta / shown), `success_rate_pct` (success / shown) |

## revenue

> Actual revenue from App Store Server Notifications (in the base currency) split by entry context, product and storefront, plus the distribution of days from install to purchase; context comes from appAccountToken attribution, missing attribution is recorded as unattributed; sales only counts transactions with price > 0, free-trial starts (subscriptions started at price 0 without an offer code) are listed separately as trials; daily is aggregated per day (reporting time zone) and only contains days with transactions

Extra parameters: none. Uses the app's `bundle_id` and Apple's environment matching `env`.

| Table | Columns |
|---|---|
| `summary`, `by_context`, `by_product`, `by_storefront`, `daily` | `key` (`all`, the context, product ID, storefront, or day), `sales`, `trials`, `refunds`, `net` (sales minus refunds, base currency), `unconverted` (transactions without an exchange rate, excluded from `net`) |
| `days_to_purchase` | `install_day` (`1`, `2-3`, `4-7`, `8-30`, `31+`; the acquisition day is day 1), `sales` (attributed first purchases with price > 0) |

## distribution

> Distribution of active users in the range by device model, iOS version, app version, region, App Store storefront and language (latest attributes of each user)

Extra parameters: none.

| Table | Columns |
|---|---|
| `device`, `os_version`, `app_version`, `region`, `storefront`, `language` | `value` (`(unknown)` when missing; devices use marketing names), `users`, `share_pct` |

## overview

> Overview KPIs for the current period and the preceding period of equal length: average DAU, active users in the range, truly new users, stickiness (average DAU / MAU as of `to`), sessions, daily sessions per user, median session duration, net revenue; plus a daily trend and user tiers

Extra parameters: none.

| Table | Columns |
|---|---|
| `kpis` | `key`, `current`, `previous`; keys: `avg_dau`, `active_users`, `new_users`, `stickiness_pct`, `sessions`, `sessions_per_user_day`, `median_session_s`, `net` |
| `period` | `from`, `to`, `prev_from`, `prev_to` |
| `daily` | `day`, `dau`, `wau`, `mau`, `new_users`, `sessions`, `median_session_s`, `net` |
| `tiers` | `tier`, `users` (active users in the range by tier, see `users`) |

## sessions_daily

> Daily sessions, active users, sessions per user, and the median / P90 foreground duration (seconds) of sessions that ended that day

Extra parameters: none.

| Table | Columns |
|---|---|
| `daily` | `day`, `sessions`, `users`, `sessions_per_user`, `median_session_s`, `p90_session_s` |

## users

> Active users in the range; tiers are based on active days in the 28 days up to `to` (heavy >= 15, medium 5-14, light 2-4, once 1, dormant 0); parameters tier / paid=1 (is_paid) / pro=1 (is_pro, current member as last reported by the app) / new=1 / sort / offset / limit

Extra parameters:

| Parameter | Values |
|---|---|
| `tier` | `heavy`, `medium`, `light`, `once`, `dormant` (or `all`) |
| `paid` | `1`: only users with `is_paid = 1` (a paid App Store transaction, `revenue_sign = +1` and price > 0, attributed to them through `appAccountToken`; free-trial starts do not count) |
| `pro` | `1`: only users with `is_pro = 1`: current members as last reported by the app (`is_pro` = `"true"` or `"1"` in the latest `user.snapshot`), or users with an App Store purchase or trial start confirmed after that snapshot and not since expired, refunded or revoked. Unlike `paid`, this includes members who bought before the SDK was integrated or restored a purchase; it is only as current as the latest snapshot |
| `new` | `1`: only truly new users |
| `sort` | `last_seen` (default), `first_seen`, `active_days`, `sessions`, `duration` |
| `offset` | Paging offset |
| `limit` | Page size, default 50, maximum 500 |

| Table | Columns |
|---|---|
| `summary` | `total` (after filters), `offset`, `limit`, `all_users`, `paid_users`, `new_users`, `pro_users` |
| `tiers` | `tier`, `users` (before filters) |
| `users` | `install_id`, `tier`, `days_28`, `first_seen_day`, `acquired_day`, `last_seen_day`, `active_days`, `sessions`, `duration_s`, `device_name`, `os_version`, `last_version`, `region`, `storefront`, `is_new`, `is_paid`, `is_pro` |

## event_trend

> Count and users of any event by day / week / month, optionally grouped by one parameter (at most 8 groups, the rest as (other)); parameters event / group_by / granularity=day|week|month

Extra parameters:

| Parameter | Values |
|---|---|
| `event` | Event name; default is the most frequent event that is not an SDK built-in (`screen.*`, `session.*`, `app.*`, `user.snapshot`) |
| `group_by` | A param key (letters, digits, `_`); values missing the param show as `(none)` |
| `granularity` | `day` (default), `week` (Monday), `month` |

| Table | Columns |
|---|---|
| `selected` | `event`, `group_by`, `granularity` (the effective choices) |
| `buckets` | `bucket` (every bucket in the range, including empty ones) |
| `series` | `bucket`, `value`, `count`, `users` |
| `breakdown` | `value`, `count`, `users` |
| `params` | `key`, `count`, `distinct_values` (params seen on the event, for picking `group_by`) |
| `events` | `name`, `count`, `users` (all events in the range) |

## errors

> error events aggregated by id: count, affected users, versions involved, last occurrence and latest message; plus daily error counts

Extra parameters: none.

| Table | Columns |
|---|---|
| `summary` | `count`, `users`, `kinds` (distinct ids) |
| `daily` | `day`, `count`, `users` (every day in the range) |
| `errors` | `id`, `category`, `count`, `users`, `versions` (newest first), `last_time_local`, `last_message` |

## portfolio

> Portfolio dashboard: proceeds of all apps (App Store Connect sales reports, after commission and taxes, in the base currency) month to date / same period last month / last month / year to date / last 12 months; per-app comparison (proceeds, downloads, MAU); within the selected range (range=week last 7 days / month last 30 days / year last 12 months / custom uses from-to) the trend by day or month x app, revenue mix (one-time purchase / subscription / paid download) and country breakdown; plus the latest live sales feed

Covers every app in the App Store Connect account; `app` and `env` do not filter it. Requires App Store Connect sync ([app-store-setup.md](app-store-setup.md)). Report days are Pacific Time days, as Apple defines them; "month to date" ends on the newest report day.

Extra parameters: `range` = `week`, `month`, `year` (default) or `custom` (uses `from`/`to`; aggregated by month when longer than 92 days).

| Table | Columns |
|---|---|
| `status` | `latest_report_day`, `earliest_month`, `synced`. When nothing is synced yet, only `status` and `feed` are returned |
| `kpis` | `key`, `current`, `previous`; keys: `mtd_proceeds`, `last_month_proceeds`, `ytd_proceeds`, `last_12m_proceeds`, `all_time_proceeds`, `mtd_downloads`, `mtd_paid_units`, `mtd_refunds` (previous = same days last month where present), `live_after_report` (current = real-time net amount from notifications after the newest report day, previous = number of paid transactions) |
| `period` | `this_month`, `through_day`, `last_month`, `last_month_same_day`, `range`, `range_from`, `range_to`, `granularity` |
| `trend` | `bucket`, `bundle_id`, `proceeds`, `downloads` |
| `apps` | `bundle_id`, `app` (configured key, if any), `asc_name`, `mtd_proceeds`, `prev_same_proceeds`, `last_month_proceeds`, `last_12m_proceeds`, `share_12m_pct`, `mtd_downloads`, `prev_same_downloads`, `mau_30d`, `new_users_30d`, `analytics` (whether the app sends SDK events) |
| `structure` | `kind` (`subscription_new`, `subscription_renewal`, `iap`, `paid_download`, `other`), `proceeds`, `units` |
| `countries` | `country`, `proceeds`, `downloads` (top 12) |
| `daily` | `day`, `proceeds`, `downloads` (last 30 report days) |
| `feed` | `time_local`, `bundle_id`, `type`, `subtype`, `product_id`, `revenue_sign`, `amount`, `storefront`, `free` (latest 30 production notifications) |

## today

> Today so far across all apps (production, reporting time zone), each compared with yesterday up to the same time of day: per app and in total, active users (distinct install_ids with any event; the total sums apps), truly new users (first download day = first seen day), sessions (session.started), paywall viewers (distinct users with paywall.shown), error events, and real-time gross from App Store notifications (price in the base currency before commission, refunds subtracted) with sales (price > 0), free-trial starts and refunds; plus active users per hour today and over all of yesterday, and today's App Store notifications

Covers every configured app plus any bundle with notifications in either window; `app`, `env`, `from` and `to` do not filter it. Uses only real-time sources (SDK events and App Store notifications), since App Store Connect reports lag about two days.

| Table | Columns |
|---|---|
| `period` | `day`, `yesterday`, `now_local` (HH:MM), `hour` |
| `kpis` | `key`, `current` (today so far), `previous` (yesterday up to the same time); keys: `gross`, `sales`, `trials`, `refunds`, `active_users`, `new_users`, `sessions`, `paywall_users`, `errors` |
| `apps` | `bundle_id`, `app` (configured key, if any), `analytics`, `active_users`, `prev_active_users`, `new_users`, `prev_new_users`, `sessions`, `prev_sessions`, `paywall_users`, `sales`, `trials`, `refunds`, `gross`, `prev_gross`, `errors` |
| `hourly` | `app`, `hour` (0-23), `today` (empty after the current hour), `yesterday` (whole day) |
| `feed` | same columns as in `portfolio`, only notifications signed today |

## store_funnel

> Store conversion (App Store Connect analytics reports): impressions (unique devices) -> product page views (unique devices) -> first-time downloads; conversion rate = first-time downloads / unique impressions (same definition as App Store Connect), product page conversion = first-time downloads / unique product page views; the window ends on the latest report day, its length is the number of days in from~to, and it is compared with the preceding window of equal length; split by app, source (search / browse / app referrer / web referrer / other) and country. scope=all covers all apps, otherwise only the current app

Requires App Store Connect analytics reports. Extra parameters: `scope=all` for all apps; otherwise the app's `bundle_id`. The window ends on the latest report day (or `to`, if earlier) and is as long as `from`..`to`.

| Table | Columns |
|---|---|
| `status` | `latest_date`, `earliest_date`, `synced`. When nothing is synced yet, only `status` is returned |
| `window` | `from`, `to`, `prev_from`, `prev_to`, `days`, `prev_complete` (whether data covers the previous window) |
| `apps` | `bundle_id`, `impressions`, `impressions_unique`, `page_views`, `page_views_unique`, `first_downloads`, `redownloads`, `view_rate`, `cvr`, `page_cvr`, `prev_impressions_unique`, `prev_first_downloads`, `prev_cvr` |
| `daily` | `date`, `bundle_id`, `impressions_unique`, `page_views_unique`, `first_downloads` |
| `sources` | `source`, `impressions_unique`, `page_views_unique`, `first_downloads`, `cvr` |
| `territories` | `territory`, `impressions_unique`, `page_views_unique`, `first_downloads`, `cvr` (top 15) |

## Single user

Not a metric but the same result shape: `GET /v1/query/user/{install_id}` / `insight user`. Definition: "Summary, activity calendar and recent events of a single anonymous user (times in the reporting time zone, newest first)". See [query-api.md](query-api.md#get-v1queryuserinstall_id).
