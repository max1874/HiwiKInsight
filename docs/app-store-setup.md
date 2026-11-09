# App Store setup

HiwiKInsight gets App Store money data from two independent sources. Set up either or both.

| Source | Latency | What it gives you | Needs |
|---|---|---|---|
| App Store Server Notifications V2 | Seconds | Every transaction (purchases, subscriptions, renewals, refunds, ...) with price, product, storefront and purchase attribution; transaction emails; real-time revenue in metrics and reports | A public HTTPS URL |
| App Store Connect API | About a day, synced every 6 hours | Sales reports (proceeds after commission and tax, downloads, refunds, full history), analytics reports (impressions, product page views, first-time downloads), the released-build list | An API key |

Every app must be listed in `config.yaml` with its `bundle_id`.

## App Store Server Notifications V2

For each app in App Store Connect: **Apps > (your app) > App Information > App Store Server Notifications**.

- **Production Server URL**: `https://insight.example.com/v1/appstore/notifications`
- **Sandbox Server URL**: `https://insight.example.com/v1/appstore/notifications`
- **Version**: Version 2

The same endpoint serves both environments; transactions are stored with Apple's `environment` (`Production` or `Sandbox`) and metrics map `env=production` / `env=sandbox` to them.

What the server does with each notification:

1. Verifies the JWS signature and certificate chain against Apple's root CA (also for the signed transaction and renewal info). Invalid signatures get HTTP 400.
2. Looks up the app by `bundleId`; unknown bundle IDs get HTTP 400.
3. Stores it in `appstore_events`, idempotently by `notificationUUID` (Apple's retries are not counted twice). `revenue_sign` is `+1` for `ONE_TIME_CHARGE`, `SUBSCRIBED`, `DID_RENEW` and `REFUND_REVERSED`, `-1` for `REFUND`, and `0` for everything else. A `+1` with price 0 is not a sale; it is a free-trial start only for `SUBSCRIBED` without an offer code. Other price-0 income, such as a product or free period redeemed with an offer code (`offerType` 3, stored as `offer_type` with the code's `offer_identifier`), is neither a trial nor a sale.
4. Converts the price to the base currency (`amount`) using the day's exchange rate. If no rate is available the amount stays empty and is reported as "unconverted".
5. Links it to the in-app purchase attempt through `appAccountToken` (see [sdk-integration.md](sdk-integration.md#how-attribution-works-end-to-end)).
6. Sends a transaction email (or logs it when mail is not configured): type, amount, conversion and estimated proceeds (`commission_rate`), source, renewal state, and today's and this month's totals.

Only a failed email returns HTTP 500, so Apple retries (after 1, 12, 24, 48 and 72 hours); everything else returns 200 or 400.

To check the setup, make a purchase with a sandbox account in a TestFlight or development build. Then:

```bash
insight sql "SELECT local_datetime(signed_at) AS time, environment, notification_type, product_id, amount FROM appstore_events ORDER BY signed_at DESC LIMIT 5"
```

## App Store Connect API

### Create a key

In App Store Connect: **Users and Access > Integrations > App Store Connect API > Team Keys**, generate a key and download the `.p8` file (it can be downloaded only once). Note the **Key ID** and the **Issuer ID**.

Access:

- Sales reports need a key whose role can read sales and financial reports (Finance, Sales, or Admin).
- The analytics reports behind the store funnel are requested through the Analytics Reports API, which Apple restricts more tightly. If the log shows `asc analytics request not created`, the key cannot create report requests; an Admin key can. Sales sync works independently of this.

The key is used read-only apart from creating analytics report requests.

### Vendor number

**Payments and Financial Reports** in App Store Connect shows your vendor number (top left).

### Configure

```yaml
app_store_connect:
  key_id: ${ASC_KEY_ID}
  issuer_id: ${ASC_ISSUER_ID}
  key_path: AuthKey_ABC123.p8        # relative to the config file
  vendor_number: ${ASC_VENDOR_NUMBER}
```

Sync starts only when all four values are present. With Docker, put the `.p8` file in `./config/` next to `config.yaml`. At startup the log shows either `app_store_connect not configured` or the first sync result (`asc sync done`, `asc analytics sync done`).

### What gets synced

| Data | Tables | Schedule and history |
|---|---|---|
| Apps (Apple ID, bundle ID, SKU, name) | `asc_apps` | Every sync |
| Summary Sales Reports, daily | `asc_sales` (`frequency = 'DAILY'`), `asc_reports` | At startup, then every 6 hours. The first run backfills the last 365 days (Apple keeps daily reports for a year); takes a few minutes |
| Summary Sales Reports, monthly | `asc_sales` (`frequency = 'MONTHLY'`) | Last 36 months; older history comes only from monthly reports |
| Analytics reports "App Store Discovery and Engagement Standard" and "App Downloads Standard" | `asc_store_engagement`, `asc_store_downloads`, `asc_analytics_instances` | Same loop. On first run HiwiKInsight creates an ongoing report request (and a one-time snapshot for history) per app; Apple starts producing data one to two days later |
| Released builds | in memory | At startup and every 15 minutes; used to reclassify TestFlight data (see [sdk-integration.md](sdk-integration.md#10-environments)) |

`asc_sales` rows are classified into `category`: `download`, `redownload`, `update`, `iap`, `subscription` or `other`. `units` are negative for refunds. `proceeds` is units times developer proceeds per unit, after Apple's commission and taxes, converted to the base currency at fetch time. In-app purchase rows are mapped to their app through the parent identifier.

### Delays and report days

- Sales reports cover whole **Pacific Time** days (Apple's definition) and are usually available the next morning Pacific Time. The newest report day is typically one to two days behind. `asc_sales.period` is that Pacific day, independent of your configured `timezone`.
- A day with no sales eventually returns 404 from Apple and is recorded as `empty` in `asc_reports`.
- Last month's monthly report appears in the first days of the next month; until then the month is summed from daily reports.
- Analytics reports lag about two to three days and each delivery may revise the previous few days.
- The portfolio metric fills the gap after the newest report day with real-time notifications (`live_after_report`).
- If the newest daily report is older than `alerts.report_stale_days` (default 3), an alert is sent; check that the key is still valid.

## Product and context names

Map product IDs and purchase contexts to readable names; they are used in emails, reports and the dashboard:

```yaml
apps:
  - key: pawprint
    bundle_id: com.example.pawprint
    products:
      com.example.pawprint.pro.lifetime: Pawprint Pro (Lifetime)
    paywall_contexts:
      record_limit: Record limit
      onboarding: Onboarding
```
