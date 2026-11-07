# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `feishu` config: charges, refunds and Apple's test notification also go to one person through a Feishu or Lark app bot.

## [v1.2.0] — 2026-10-21

### Added

- Metric `today`: today so far across all apps from real-time sources (active users, new users, sessions, paywall viewers, errors, gross, sales, trials, refunds), each compared with yesterday up to the same time of day, with active users by hour and today's notifications. The Portfolio dashboard opens on a matching **Today** range (the default; Year moves to `?range=year`) that needs no app switching and refreshes every minute.

### Changed

- The daily report no longer shows a day-over-day change for revenue (gross in the headline and per app, proceeds in the App Store table); DAU, new users, sessions and downloads keep theirs.
- Building the dashboard pins pnpm 10.18.3 through `packageManager` in `web/package.json`.
- `alerts.payment_errors` defaults to `[purchase, restore]`; `products.load` no longer alerts on its first occurrence, because an empty load at first launch is usually transient. Users who really cannot buy still alert through `purchase.failed` with reason `product_missing`. Add `products.load` back to `payment_errors` to keep the old behavior.

## [v1.1.2] — 2026-10-20

### Fixed

- Dashboard on phones: the app, event and currency selects opened a menu that the horizontally scrolling filter bar clipped, so switching apps was impossible; the menu now opens fixed below its button. Column charts with up to 8 buckets (active days, days from install to purchase) show every label instead of dropping alternate ones.

## [v1.1.1] — 2026-10-12

### Added

- Attribution for apps that pass their own stable `appAccountToken` (an account ID) on every purchase, with HiwiKInsightKit 0.3.0's `beginPurchase(product:context:appAccountToken:)`. `purchase_attempts` now keeps every attempt of a token (surrogate key `id`, deduplicated by token, install and time), and the new view `v_attribution` maps each notification to one attempt: the latest attempt with the same token in the same app started no later than 5 minutes after the purchase (for renewals, refunds and expiry: after the purchase that started them). The `funnel` and `revenue` metrics, reports, transaction emails, `is_paid` and `is_pro` all resolve through it. Tokens generated per purchase resolve exactly as before.
- `appstore_events` stores `app`, `original_transaction_id`, `transaction_reason`, `purchase_at` and `original_purchase_at` for new notifications.
- Database migration v11 rebuilds `purchase_attempts` (existing rows kept) and recreates `v_user_summary`.

## [v1.1.0] — 2026-10-11

### Added

- `v_user_summary.is_pro`: current membership from `is_pro` in the latest `user.snapshot`, or an App Store purchase or trial start confirmed after it and not since expired, refunded or revoked. Unlike `is_paid`, it includes members who bought before the SDK was integrated or restored a purchase. The `users` metric gains a `pro=1` filter, an `is_pro` column and `pro_users` in its summary; the dashboard shows a Pro badge and a "Pro only" filter, which replace the Paid badge and the "Paid only" filter (`is_paid` and `paid=1` remain in the query API for purchase attribution). Adds database migration v10 (recreates the view, no data change).

## [v1.0.0] — first open-source release

### Added

- Single Go binary (`hiwikinsight`) with embedded pure-Go SQLite (no cgo), subcommands `serve`, `demo`, `check-config`, `hash-password` and `version`.
- One YAML configuration file with `${NAME}` / `${NAME:-default}` environment expansion, strict validation, configurable reporting time zone, language (`en`/`zh`) and base currency.
- Event ingestion (`POST /v1/events`) for the HiwiKInsightKit Swift SDK (protocol schema 1): raw DEFLATE, limits, deduplication, clock correction, per-install daily cap, no IP addresses stored.
- Derived tables for installs, daily activity and purchase attempts; analysis views `v_user_days`, `v_user_summary`, `v_sessions`, `v_screen_usage`; raw event retention (`retention_days`).
- "Truly new user" detection from the App Store original download date.
- Environment safety nets: production data from never-released builds and from development devices is reclassified as sandbox.
- App Store Server Notifications V2 (`POST /v1/appstore/notifications`) with JWS verification, idempotent storage, base-currency conversion, transaction emails and purchase attribution through `appAccountToken`.
- App Store Connect sync: Summary Sales Reports (daily and monthly history), Analytics Reports (impressions, product page views, first-time downloads by source and country) and the released-build list.
- 15 built-in metrics with fixed definitions: `active_users`, `activity_days`, `retention`, `returning`, `module_usage`, `funnel`, `revenue`, `distribution`, `overview`, `sessions_daily`, `users`, `event_trend`, `errors`, `portfolio`, `store_funnel`.
- Read-only query API (`/v1/query/describe`, `meta`, `metric/{name}`, `sql`, `user/{id}`) with bearer tokens, a read-only SQLite connection and limits (single SELECT/WITH, 1000 rows, 10 s, 400-day range).
- `describe` semantic layer for AI assistants: conventions, tables, views, metrics, data volume, undocumented events and per-app event catalogs.
- `insight` CLI and a built-in MCP server (`insight mcp`) with the tools `describe`, `list_apps`, `metric`, `sql` and `user`.
- Claude Code skill `hiwikinsight-analytics`.
- Web dashboard with 13 pages (Portfolio, Overview, Engagement, Retention, Modules, Users, User detail, Acquisition, Revenue, Devices & Regions, Events, Errors, Settings), English and Chinese, light and dark mode, display currencies, bcrypt sign-in.
- Daily and weekly email reports with preview and resend endpoints.
- Alerts for failed purchases, payment errors, new and spiking errors, stale App Store Connect reports and silent apps, deduplicated per day.
- Demo mode with three sample apps and 90 days of generated data (`hiwikinsight demo`, `docker-compose.demo.yml`, `make demo`).
- Deployment assets: multi-arch container image on GHCR, Docker Compose with Caddy, systemd unit, nginx example, release archives for linux and darwin (amd64, arm64).
- Documentation in `docs/`, `AGENTS.md` for AI coding agents, English and Chinese READMEs.

[Unreleased]: https://github.com/RedHiwiK/HiwiKInsight/compare/v1.2.0...HEAD
[v1.2.0]: https://github.com/RedHiwiK/HiwiKInsight/compare/v1.1.2...v1.2.0
[v1.1.2]: https://github.com/RedHiwiK/HiwiKInsight/compare/v1.1.1...v1.1.2
[v1.1.1]: https://github.com/RedHiwiK/HiwiKInsight/compare/v1.1.0...v1.1.1
[v1.1.0]: https://github.com/RedHiwiK/HiwiKInsight/compare/v1.0.0...v1.1.0
[v1.0.0]: https://github.com/RedHiwiK/HiwiKInsight/releases/tag/v1.0.0
