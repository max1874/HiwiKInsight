# Configuration

A HiwiKInsight deployment is configured by one YAML file. The fully commented reference is [examples/config.example.yaml](../examples/config.example.yaml); this page lists every field as defined in `internal/config/config.go`.

## Finding the file

- `hiwikinsight serve -config PATH` (and `check-config -config PATH`).
- Without `-config`: `$HIWIKINSIGHT_CONFIG`, else `./config.yaml`.
- The container image sets `HIWIKINSIGHT_CONFIG=/etc/hiwikinsight/config.yaml`.

An empty file is valid and means "all defaults" (no apps, so nothing is accepted yet).

## Environment variables in values

Any value may reference an environment variable:

| Syntax | Result |
|---|---|
| `${NAME}` | The value of `NAME`, or an empty string when it is unset or empty |
| `${NAME:-default}` | The value of `NAME`, or `default` when it is unset or empty |

Expansion happens inside values after YAML parsing, so a secret containing `#`, `: ` or quotes is used literally and never changes the file's structure. An unquoted reference such as `smtp_port: ${SMTP_PORT:-587}` is typed after expansion, so it still decodes as a number. Use it for secrets (query tokens, SMTP password, App Store Connect credentials, password hashes) so the file itself can be committed or shared. Only the `${...}` form is expanded; a bare `$` (as in a bcrypt hash) is left alone.

Two process-level variables are read directly:

| Variable | Effect |
|---|---|
| `HIWIKINSIGHT_CONFIG` | Default config path when `-config` is not given |
| `HIWIKINSIGHT_DATA_DIR` | Default `data_dir` when the config does not set one (the container image sets `/data`) |

## Paths

Relative paths in the file (`data_dir`, `apps[].catalog`, `app_store_connect.key_path`) are resolved against the directory that contains the config file, not the working directory.

## Validation

```bash
hiwikinsight check-config -config config.yaml
```

prints the effective settings and fails on errors. Validation is strict:

- Unknown keys are rejected (a typo is an error, not a silently ignored setting).
- `timezone` must be a valid IANA zone; `language` must be `en` or `zh`; `currency` must be three letters; `commission_rate` must be in `[0, 1)`; `reports.hour` must be 0 to 23.
- `apps[].key` must match `^[a-z0-9][a-z0-9_-]{0,63}$` and be unique; `apps[].bundle_id` is required and unique; `apps[].color` must be one of the palette names.
- Every `dashboard.users[]` entry needs a `username` and a bcrypt `password_hash` (starting with `$2`).
- `check-config` also checks that every catalog file exists.
- The server additionally refuses to start when `dashboard.insecure_no_auth` is true and `listen` is not a loopback address.

## Top-level fields

| Field | Type | Default | Description |
|---|---|---|---|
| `listen` | string | `:8080` | HTTP listen address, e.g. `:8080` or `127.0.0.1:8080` behind a reverse proxy |
| `public_url` | string | empty | How users and the SDK reach the server, e.g. `https://insight.example.com`. Informational; see the note below |
| `data_dir` | path | `$HIWIKINSIGHT_DATA_DIR`, else `./data` | Directory for the database `hiwikinsight.db` and the session secret `session.key`. Created if missing |
| `timezone` | IANA zone | `UTC` | Defines a "day" for every metric, report and alert |
| `language` | `en` or `zh` | `en` | Language of transaction emails, reports and alerts (the dashboard picks its own language per browser) |
| `currency` | ISO 4217 | `USD` | Base currency that revenue is converted to and stored in |
| `commission_rate` | float | `0.15` | Apple's commission used to estimate proceeds in transaction emails: `0.15` for the Small Business Program, `0.30` otherwise. `0` means "use the default" |
| `retention_days` | int | `365` | Days raw events are kept (at least 1). Derived tables are kept forever (see [privacy.md](privacy.md)) |
| `apps` | list | empty | Apps that send events and/or App Store data. See below |
| `dashboard` | object | | Web dashboard and sign-in |
| `query` | object | | Query API tokens |
| `ingest` | object | | Event ingestion limits |
| `mail` | object | | SMTP settings for all email |
| `feishu` | object | | Feishu or Lark messages for charges and refunds |
| `reports` | object | | Daily and weekly reports |
| `alerts` | object | | Alert rules |
| `app_store_connect` | object | | App Store Connect API credentials |

Note: `public_url` is informational. `check-config` prints it together with the App Store notification URL to configure, and the server logs it at startup; nothing else depends on it.

## `apps[]`

| Field | Type | Default | Description |
|---|---|---|---|
| `key` | string | required | Identifies the app everywhere: the SDK's `appKey`, `?app=` in the query API, `--app` in the CLI. Lowercase letters, digits, `-` and `_` |
| `bundle_id` | string | required | Links App Store Server Notifications and App Store Connect reports to the app |
| `name` | string | `key` | Display name in the dashboard, emails and reports |
| `color` | string | next free slot | Dashboard palette slot: `purple`, `teal`, `orange`, `pink`, `blue`, `violet`, `green`, `gold`. Apps without a color get the remaining slots in config order |
| `catalog` | path | none | The app's event catalog (YAML). See [event-catalog.md](event-catalog.md) |
| `products` | map | empty | Product ID to display name, used in emails, reports and the dashboard |
| `paywall_contexts` | map | empty | The SDK's purchase `context` values to display names |

The first app in the list is the default app for the query API and CLI when `app` is omitted.

Events for an app key that is not listed are rejected with HTTP 400. App Store notifications for an unknown `bundle_id` are rejected with HTTP 400.

## `dashboard`

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | Serve the dashboard under `/dashboard/` (and redirect `/` there) |
| `users` | list | empty | Users who can sign in: `{username, password_hash}`. Create a hash with `hiwikinsight hash-password` (minimum 8 characters) |
| `session_ttl` | Go duration | `720h` (30 days) | How long a sign-in lasts, e.g. `720h`, `24h`, `30m` |
| `insecure_no_auth` | bool | `false` | Disable sign-in entirely. Only allowed when `listen` is a loopback address such as `127.0.0.1:8080` |

Removing a user from the config invalidates that user's sessions at the next request. Deleting `session.key` in the data directory signs everyone out.

## `query`

| Field | Type | Default | Description |
|---|---|---|---|
| `tokens` | list of strings | empty | Tokens accepted as `Authorization: Bearer <token>` on `/v1/query/*`. Empty strings are ignored. Without tokens the query API is only reachable from a signed-in dashboard session |

Generate tokens with `openssl rand -hex 32`. Use one token per client so you can revoke them independently.

## `ingest`

| Field | Type | Default | Description |
|---|---|---|---|
| `daily_cap` | int | `5000` | Maximum events accepted per install per day (reporting time zone). The excess is dropped silently and still acknowledged with 202 so the SDK does not retry |

## `mail`

| Field | Type | Default | Description |
|---|---|---|---|
| `smtp_host` | string | empty | SMTP server. Mail is sent only when `smtp_host` and `to` are both set; otherwise each message is only logged |
| `smtp_port` | int | `465` | `465` uses implicit TLS; any other port (usually `587`) uses STARTTLS |
| `username` | string | empty | SMTP user |
| `password` | string | empty | SMTP password; use `${SMTP_PASSWORD}` |
| `from` | string | `username` | Sender address |
| `to` | list | empty | Recipients of transaction emails, reports and alerts |

## `feishu`

Messages are sent only when `app_id`, `app_secret` and `open_id` are all set; setting only some of them is an error.

| Field | Type | Default | Description |
|---|---|---|---|
| `app_id` | string | empty | App with a bot and the `im:message:send_as_bot` permission |
| `app_secret` | string | empty | Use `${FEISHU_APP_SECRET}` |
| `open_id` | string | empty | Recipient's `open_id` under this app; another app's `open_id` is rejected |
| `base_url` | string | `https://open.feishu.cn` | `https://open.larksuite.com` for Lark |

## `reports`

| Field | Type | Default | Description |
|---|---|---|---|
| `daily` | bool | `true` | Send yesterday's daily report |
| `weekly` | bool | `true` | Send last week's report on Mondays |
| `hour` | int 0-23 | `9` | Hour in the reporting time zone after which reports are sent |

See [reports-and-alerts.md](reports-and-alerts.md).

## `alerts`

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | Run the alert checks every 5 minutes |
| `spike_users` | int | `3` | An error seen before alerts when it affects at least this many users within an hour |
| `report_stale_days` | int | `3` | Alert when the newest App Store Connect daily report is older than this many days |
| `silent_hours` | int | `48` | Alert when an app that had events in the previous 7 days sends nothing for this many hours |
| `payment_errors` | list | `[purchase, restore]` | Error ids (the `id` of `error` events) that alert on their first occurrence each day. Set `[]` to disable |

## `app_store_connect`

Sync is enabled only when all four values are present.

| Field | Type | Default | Description |
|---|---|---|---|
| `key_id` | string | empty | API key ID |
| `issuer_id` | string | empty | Issuer ID |
| `key_path` | path | empty | The `.p8` private key file |
| `vendor_number` | string | empty | Vendor number from Payments and Financial Reports |

See [app-store-setup.md](app-store-setup.md).

## Minimal example

```yaml
timezone: America/New_York
currency: USD

apps:
  - key: pawprint
    bundle_id: com.example.pawprint
    name: Pawprint
    catalog: catalogs/pawprint.yaml

dashboard:
  users:
    - username: admin
      password_hash: ${HIWIKINSIGHT_ADMIN_HASH}

query:
  tokens:
    - ${HIWIKINSIGHT_QUERY_TOKEN}
```
