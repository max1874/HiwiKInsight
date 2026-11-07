# Privacy

HiwiKInsight is built so that you can analyze how your app is used without collecting personal data, and without sending anything to a third party. You run the server; the data stays in your SQLite file.

## What the SDK sends

- **An anonymous install id**: a random UUID generated on first launch and stored in `UserDefaults`. It is not derived from any device identifier, is neither the IDFA nor the IDFV, and changes when the app is reinstalled.
- **Batch context**: app version and build, OS and version, device model identifier (such as `iPhone17,1`), locale, language, region setting, App Store storefront country, environment (production, TestFlight or development), appearance (light/dark) and Dynamic Type size.
- **Events**: built-in lifecycle, session and screen events, plus the events and parameters your app chooses to send.
- **App Store original download time** (from `AppTransaction`), to tell new users from existing ones.
- **A purchase token**: the UUID passed as StoreKit's `appAccountToken`, to attribute purchases. By default it is random per purchase. Apps that pass their own fixed account ID instead make the installs of one account linkable through this token; HiwiKInsight does not use it for anything but attribution.

## What is not collected

- No names, emails, phone numbers, contacts, location, photos, or advertising identifiers.
- No IP addresses: the ingest endpoint never reads the client address or forwarding headers, and nothing derived from the IP is stored with events.
- No cross-app or cross-device identity: install ids are per app install.
- No third-party services: events go only to your server.

Your own event parameters are your responsibility. Send bucketed, non-identifying values (`"2-5"`, `"21-100"`) and never free text a user typed, and keep error messages free of personal data.

## App Store data

App Store Server Notifications contain transaction data from Apple (product, price, currency, storefront, transaction ids, the `appAccountToken`), not the buyer's identity. App Store Connect reports are aggregated by day, product and country.

## Where IP addresses can still appear

- **Reverse proxy access logs** (Caddy, nginx) log client IPs by default. Disable or anonymize the access log for `/v1/events` if you want no IPs at all.
- **Dashboard sign-in throttling** keeps failed attempts per client IP in memory for 15 minutes; it is never written to disk.
- **Rejected App Store notifications** (invalid signature) are logged with the `X-Real-IP` header.

## Outbound connections from the server

| Destination | Purpose | Data sent |
|---|---|---|
| `open.er-api.com` | Exchange rates | The base currency code |
| `api.appstoreconnect.apple.com` | Sales and analytics reports, released builds (only if configured) | Your API credentials |
| Your SMTP server | Emails (only if configured) | Reports, alerts, transaction details |
| Feishu or Lark (`feishu.base_url`) | Charge and refund messages (only if configured) | App name, product, amount, storefront, revenue totals |

## Retention

| Data | Kept |
|---|---|
| Raw events (`events`) | `retention_days` (default 365), purged daily |
| Installs, daily activity, purchase attempts | Forever (compact, needed for long-term retention and attribution) |
| App Store transactions and App Store Connect reports | Forever |

Views built on raw events (`v_sessions`, `v_screen_usage`) and metrics that read raw events (`module_usage`, `funnel`, `event_trend`, `errors`) only cover the retention window.

There is no built-in endpoint to delete a single install's data. If you need to honor a deletion request, stop the server and delete the rows for that `install_id` from `events`, `installs`, `daily_active` and `purchase_attempts` with the `sqlite3` CLI. In the app, `HiwiKInsight.setEnabled(false)` stops collection and deletes unsent events, which you can wire to an in-app opt-out.

## App Store privacy labels

What you declare depends on what your app sends. With the built-in events and bucketed parameters, the data is usage data and diagnostics (product interaction, crash/error data, performance), collected for analytics and not linked to the user's identity or used for tracking. Review your own custom events before filling in the labels.
