# Architecture

HiwiKInsight is a single Go process with a single SQLite database. This page describes its components, how data flows through them, the schema, and the main design decisions.

## Components

```
cmd/hiwikinsight            server binary (serve, demo, check-config, hash-password, version)
  internal/server           builds everything from the config, owns routes and background jobs
    internal/ingest         POST /v1/events
    internal/notify         POST /v1/appstore/notifications  (internal/appstore verifies JWS)
    internal/query          /v1/query/*                      (internal/metrics computes metrics)
    internal/auth           /v1/auth/*, sessions
    internal/dashboard      /dashboard/*                     (web/ build output, embedded)
    internal/report         daily/weekly reports, alerts     (internal/mailer sends)
    internal/asc            App Store Connect sync, released builds
  internal/store            SQLite: migrations, writer queue, queries
  internal/tz, fx, i18n     time zone + SQL functions, exchange rates, language
  internal/catalog          event catalogs
cmd/insight                 CLI and MCP server; talks to /v1/query/* over HTTP only
```

### HTTP routes

| Route | Handler |
|---|---|
| `POST /v1/events` | `ingest.Handler` |
| `POST /v1/appstore/notifications` | `notify.Handler` |
| `GET /v1/query/describe`, `GET /v1/query/meta`, `GET /v1/query/metric/{name}`, `POST /v1/query/sql`, `GET /v1/query/user/{id}` | `query.Handler` (bearer token or session) |
| `POST /v1/auth/login`, `POST /v1/auth/logout`, `GET /v1/auth/me` | `auth.Auth` |
| `GET /v1/admin/report/preview`, `POST /v1/admin/report/send` | `server` (session only) |
| `GET /dashboard/...`, `GET /` (redirect) | `dashboard.Handler` (when enabled) |
| `GET /healthz` | `ok` |

### Background jobs

| Job | Interval | Notes |
|---|---|---|
| Event writer | 1 s or 500 events | Single goroutine draining a 1024-batch queue into SQLite |
| Raw event purge | Every 24 h | Deletes `events` older than `retention_days` |
| Report scheduler | Every 5 min | Sends due daily/weekly reports once |
| Alerter | Every 5 min | Production-only checks, deduplicated per day |
| App Store Connect sync | At start, then every 6 h | Sales reports, then analytics reports |
| Released builds | At start, then every 15 min | Environment fallback for TestFlight builds |
| Mixed-install reclassification | At start | Moves production data of development devices to sandbox |
| Exchange rates | Refreshed once per day on demand | `open.er-api.com`, last rates kept if unavailable |

## Data flow

**Events.** The SDK posts compressed batches to `/v1/events`. `ingest` checks the app key against `apps[].key`, validates and truncates fields (protocol limits), keeps only allow-listed context keys, corrects the clock using `sent_at`, applies the per-install daily cap and the released-build environment fallback, and enqueues the batch (HTTP 202). The writer goroutine stores each batch in one transaction:

- `events`: the raw event (deduplicated by `event_id`), with `day` computed in the reporting time zone.
- `daily_active`: one row per app, env, day and install, with session count, foreground seconds and event count.
- `installs`: first seen, acquisition time (minimum of first seen and the App Store original download time; one day earlier when `prior_usage=1`), latest version, device, OS, region, storefront, language.
- `purchase_attempts`: one row per `purchase.started` (a token can have many when the app passes its own account ID).

**App Store notifications.** `notify` verifies the signed payload, transaction and renewal info against Apple's root CA, maps `bundleId` to an app, stores the transaction in `appstore_events` (idempotent by `notificationUUID`) with its base-currency `amount`, `app_account_token`, original transaction ID, transaction reason and purchase times, resolves its purchase attempt through `v_attribution` (waiting up to 5 s for first purchases), and sends the transaction email.

**App Store Connect.** `asc` lists the account's apps, downloads missing daily and monthly Summary Sales Reports into `asc_sales` (logged in `asc_reports`), and downloads analytics report instances into `asc_store_engagement` and `asc_store_downloads`.

**Reads.** The query API, metrics, reports and alerts all read through a separate read-only connection pool.

## Tables and views

| Table | Grain | Retention |
|---|---|---|
| `events` | One raw SDK event: `event_id`, `app`, `env`, `install_id`, `session_id`, `name`, `ts`, `day`, `received_at`, context columns, `params` (JSON text) | `retention_days` |
| `installs` | One per app and install | Forever |
| `daily_active` | App, env, day, install: `sessions`, `duration_s`, `events` | Forever |
| `purchase_attempts` | One per `purchase.started` (`id`, `token`): app, env, install, context, product, time, version, device, OS | Forever |
| `appstore_events` | One per App Store notification: type, subtype, product, transaction, storefront, currency, `price_milli`, `revenue_sign`, `amount`, `signed_at`, `app_account_token`, `app`, `original_transaction_id`, `transaction_reason`, `purchase_at`, `original_purchase_at`, `offer_type`, `offer_identifier` | Forever |
| `asc_apps` | Apps in the App Store Connect account | Forever |
| `asc_reports` | Fetch log of sales reports (`ok`, `empty`, ...) | Forever |
| `asc_sales` | Summary Sales Report rows: period, month, bundle, SKU, category, units, `proceeds`, country, subscription | Forever |
| `asc_analytics_instances` | Processed analytics report instances | Forever |
| `asc_store_engagement` | Date, bundle, source, territory: impressions and product page views (total and unique) | Forever |
| `asc_store_downloads` | Date, bundle, source, territory: first downloads, redownloads | Forever |
| `report_sends` | Sent reports and alerts (`kind`, `period`) | Forever |
| `device_models` | Model identifier to marketing name; reloaded from the binary at every start | |

| View | Purpose |
|---|---|
| `v_user_days` | `daily_active` under an analysis-friendly name |
| `v_attribution` | One row per notification with an `appAccountToken`: `notification_uuid`, `attempt_id` (`purchase_attempts.id`, NULL when unattributed); the only place the matching rule lives |
| `v_user_summary` | One row per user: first seen, acquired and last seen days, `is_new`, versions, device name, OS, region, storefront, language, active days, sessions, duration, `is_paid` (attributed paid sale), `is_pro` (current member, from the latest `user.snapshot`) |
| `v_sessions` | One row per session: start, day, `source`, `duration_s`, screen views, events |
| `v_screen_usage` | Screen views and exits: `kind` (`viewed`/`left`), `screen`, `module`, `duration_s` |

The authoritative schema is `internal/store/migrations.go`; `describe` documents the columns for queries.

## Time zone model

A "day" is a calendar day in the configured `timezone` everywhere: `events.day`, `daily_active.day`, cohorts, metric ranges, report periods, alert windows and the daily cap. Timestamps (`ts`, `*_at`) are stored as UTC milliseconds. Package `tz` holds the zone and registers SQLite functions so SQL never hard-codes an offset and stays correct across daylight-saving changes:

| Function | Returns |
|---|---|
| `local_date(ms)` | `YYYY-MM-DD` of a millisecond timestamp |
| `local_datetime(ms)` | `YYYY-MM-DD HH:MM:SS` |
| `local_today()` | Today's date |

`events.day` is computed when the event is written. Changing `timezone` later affects new events and every computation using `local_date()`, but existing `events.day` and `daily_active.day` values keep the zone they were written in.

App Store Connect sales report days (`asc_sales.period`) are Pacific Time days, as Apple defines them, and are not converted.

## Currency model

Everything monetary is stored in the base `currency`:

- `appstore_events.amount`: the customer price (`price_milli / 1000` in the transaction currency) converted at the time the notification is received, before Apple's commission. `NULL` when no rate is available (reported as `unconverted`).
- `asc_sales.proceeds`: units times developer proceeds per unit (after commission and taxes) converted at fetch time.
- The dashboard converts base amounts to a display currency with current rates; stored values never change.

## Design decisions

- **SQLite, single writer.** One file is easy to run and back up, and is plenty for indie-scale traffic. The read-write connection pool has one connection; ingestion goes through a queue and a single writer goroutine that batches writes into transactions. WAL mode lets readers run concurrently with the writer. A full queue returns HTTP 503 so the SDK backs off.
- **Read-only connection for queries.** The query API, metrics and reports use a separate pool opened with `mode=ro` and `query_only(1)` (4 connections). Together with `ValidateSQL` (one `SELECT`/`WITH`) this makes arbitrary SQL from AI assistants safe.
- **Pure Go, no cgo.** `modernc.org/sqlite` allows static binaries for linux and darwin on amd64 and arm64 and a distroless image.
- **Derived tables kept forever, raw events expire.** Metrics over long periods (retention, active days, attribution) use compact derived tables, so raw events can be purged without losing history.
- **Fixed metric definitions in one place.** The dashboard, API, CLI, MCP and reports share `internal/metrics`, and each result carries its definition, so people and agents agree on what a number means.
- **Semantic layer generated from the live server.** `describe` combines static conventions with the current apps, metrics, data volume, undocumented events and event catalogs.
- **Idempotency everywhere.** Events by `event_id`, notifications by `notificationUUID`, sales reports replaced per period, analytics data replaced per app and date, reports and alerts recorded in `report_sends`.
- **Append-only migrations** tracked with `PRAGMA user_version`.
- **One config file**, strictly validated (unknown keys are errors), with environment expansion for secrets.
