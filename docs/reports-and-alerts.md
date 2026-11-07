# Reports and alerts

HiwiKInsight sends three kinds of email, all to `mail.to` and in the `language` of the config (`en` or `zh`):

1. A **transaction email** for every App Store Server Notification.
2. **Daily and weekly reports.**
3. **Alerts** for problems that need attention.

Every email has an HTML and a plain-text part. Without `mail.smtp_host` and `mail.to`, nothing is sent and each message's subject is written to the log instead, so you can try everything before configuring SMTP.

```yaml
mail:
  smtp_host: smtp.example.com
  smtp_port: 465            # 465 = implicit TLS; any other port uses STARTTLS
  username: insight@example.com
  password: ${SMTP_PASSWORD}
  from: insight@example.com # defaults to username
  to: [you@example.com]
```

## Transaction emails

Sent as each notification arrives (see [app-store-setup.md](app-store-setup.md)): notification type in plain words (purchase, new subscription, renewal, refund, expired, ...), the amount in the customer's currency, the conversion to the base currency and the estimated proceeds after `commission_rate`, the product, storefront, the purchase source found through `appAccountToken` (paywall context, install day, paywall views, app version, device), renewal information, and today's and this month's totals for the app. Sandbox notifications are marked as such.

If sending fails, the notification endpoint returns HTTP 500 so Apple retries later; the transaction itself is already stored and is not counted twice.

## Feishu messages

With [`feishu`](configuration.md#feishu) configured, charges, refunds and Apple's test notification also go to one person through a Feishu or Lark bot, with today's and this month's totals. Free trial starts count, because they arrive as new subscriptions at price zero. Other notifications, reports and alerts stay in email. A failed message also returns HTTP 500, so while Feishu is down a retry can repeat the email.

## Daily and weekly reports

| Setting | Default | Meaning |
|---|---|---|
| `reports.daily` | `true` | Send the report for yesterday |
| `reports.weekly` | `true` | Send the report for last week (Monday to Sunday) on Mondays |
| `reports.hour` | `9` | Earliest hour (reporting time zone) to send |

The scheduler checks every 5 minutes. Once the hour has passed, it sends any due report that is not yet recorded in the `report_sends` table, so restarts and deploys neither resend nor skip a day. A failed send is retried after 30 minutes.

**Daily report** (subject `Daily Report <day> · Gross <amount> · DAU <n>`):

- Yesterday's gross revenue from real-time App Store notifications (before commission), sales, trials and refunds; gross is shown without a day-over-day change.
- Total DAU, truly new users and sessions across apps.
- Per app: DAU, new users, sessions, paywall views to sales, gross.
- The day's transactions.
- Downloads and proceeds of the newest App Store Connect report day (usually about two days behind), when synced.
- Errors first reported that day.

**Weekly report** (subject `Weekly Report <range> · Proceeds <amount> · WAU <n>`, or `Gross` when App Store Connect reports do not cover the week yet):

- Last week's proceeds from App Store Connect (after commission and tax) with month to date and real-time gross, or real-time gross only.
- Weekly active users, new users and average DAU vs. the previous week.
- Per app: average DAU, WAU, new users, stickiness, money.
- New-user retention (day 1 and day 7) of the previous week's truly new users.
- Paywall funnel: paywall views, buy taps, verified sales, trials.
- Store conversion, when analytics reports are synced.
- Top 5 errors.

Comparisons are shown only when the previous period has data.

### Preview and resend

Both endpoints need a signed-in dashboard session (query tokens are not accepted). The Settings page links to the previews.

| Request | Effect |
|---|---|
| `GET /v1/admin/report/preview?kind=daily` | Render yesterday's daily report as HTML |
| `GET /v1/admin/report/preview?kind=weekly` | Render last week's report |
| `...&period=YYYY-MM-DD` | A specific day, or the Monday of a specific week |
| `...&format=text` | Plain text with the subject line |
| `POST /v1/admin/report/send?kind=daily&period=YYYY-MM-DD` | Send that report now. Not recorded, so the scheduled send is unaffected. Both parameters are required |

## Alerts

Enabled by `alerts.enabled` (default `true`). Every 5 minutes the alerter checks **production data only** and emails each problem it finds. The same problem (rule + app + id) is sent at most once per day (reporting time zone), recorded in `report_sends` as `alert:<rule>`. A failed send is retried at the next check.

| Rule | Fires when | Threshold |
|---|---|---|
| `purchase` | A `purchase.failed` event arrived today with a `reason` other than `user_cancelled` or `pending`; one alert per app and reason | First occurrence of the day |
| `error` (payment) | An `error` event arrived today whose `id` is in `alerts.payment_errors` | `payment_errors`, default `purchase`, `restore` |
| `error` (new) | An `error` id arrived today that was never seen before | First occurrence ever |
| `error` (spike) | An error id seen before affected many users in the last hour | `alerts.spike_users`, default 3 users |
| `asc_stale` | The newest App Store Connect daily sales report is older than the threshold (only once reports have been synced) | `alerts.report_stale_days`, default 3 days |
| `silent` | An app that sent production events during the previous 7 days has sent none for the threshold | `alerts.silent_hours`, default 48 hours |

Each purchase and error alert lists up to 10 recent occurrences: time, detail (product or error message) and context, device, OS, app version, storefront, and the first 8 characters of the install id (open the full timeline on the dashboard's Users page). Purchase alerts add a hint for known reasons (`product_missing`: products did not load from the App Store; `verification`: transaction verification failed; `system_error`: StoreKit or network error).

To make these alerts useful, send `purchase.failed` with a `reason`, and `error` events with stable ids (see [sdk-integration.md](sdk-integration.md)).

```yaml
alerts:
  enabled: true
  spike_users: 3
  report_stale_days: 3
  silent_hours: 48
  payment_errors: [purchase, restore]
```

`products.load` is not in the default list: an empty product load at first launch is often transient and recovers on the next load. A user who really cannot buy shows up as `purchase.failed` with reason `product_missing`, which the `purchase` rule already alerts on.
