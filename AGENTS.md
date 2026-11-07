# AGENTS.md

Guidance for AI coding agents (and people) working on the HiwiKInsight repository. For using the data of a running HiwiKInsight server from an agent, see [docs/ai-integration.md](docs/ai-integration.md) instead.

HiwiKInsight is a self-hosted analytics and App Store revenue server: one Go binary (`cmd/hiwikinsight`), pure-Go SQLite, an embedded React dashboard (`web/`), and a read-only query CLI with an MCP server (`cmd/insight`).

## Repo map

| Path | Purpose |
|---|---|
| `cmd/hiwikinsight` | Server entry point: `serve`, `demo`, `check-config`, `hash-password`, `version` |
| `cmd/insight` | Query CLI (`describe`, `metric`, `sql`, `user`, `apps`) and the stdio MCP server (`mcp.go`); stdlib only |
| `internal/config` | Loads and validates the single YAML config; env expansion, defaults, path resolution |
| `internal/server` | Wires everything: routes, background jobs, report preview/send endpoints |
| `internal/ingest` | `POST /v1/events`: decodes SDK batches, enforces limits, daily cap, clock correction |
| `internal/store` | SQLite access: migrations, single-writer queue, analytics writes, App Store and ASC tables, reclassification |
| `internal/query` | `/v1/query/*`: describe (semantic layer), metric, sql, user, meta; bearer-token auth |
| `internal/metrics` | Built-in metric definitions and computation, shared by the query API, dashboard and reports |
| `internal/notify` | `POST /v1/appstore/notifications`: verify, record, attribute, transaction email |
| `internal/appstore` | JWS verification of App Store notifications against the Apple root CA; payload types |
| `internal/asc` | App Store Connect API client: sales reports, analytics reports, released-build list |
| `internal/report` | Daily/weekly report builder and scheduler; alert rules and alerter |
| `internal/mailer` | SMTP sender (465 implicit TLS, otherwise STARTTLS) and a log-only sender |
| `internal/feishu` | Feishu or Lark bot sender for charge and refund messages |
| `internal/auth` | Dashboard sign-in: bcrypt users, HMAC session cookie, failure throttling |
| `internal/catalog` | Loads per-app event catalogs (YAML text, lightly parsed) |
| `internal/tz` | Reporting time zone and the SQL functions `local_date`, `local_datetime`, `local_today` |
| `internal/fx` | Exchange rates to the base currency (open.er-api.com, cached per day) |
| `internal/i18n` | Language of server-generated text: `i18n.T(en, zh)` |
| `internal/dashboard` | Serves the embedded dashboard build under `/dashboard/` |
| `internal/demo` | Demo mode: sample apps, catalogs and 90 days of generated data |
| `web/` | Dashboard (React, TypeScript, Vite, Tailwind, ECharts); `web/embed.go` embeds `web/dist` |
| `skills/` | Claude Code skill for analysing data with the `insight` CLI |
| `examples/` | Reference config and example event catalogs |
| `deploy/` | Caddyfile, `.env.example`, systemd unit, nginx config |
| `docs/` | User and developer documentation |

## Build and test

Requirements: Go 1.26+, Node 22, pnpm 10.

```bash
make web            # build the dashboard into web/dist/app (embedded into the binary)
make build          # bin/hiwikinsight and bin/insight (CGO_ENABLED=0)
go test ./...       # Go tests
go vet ./...
cd web && pnpm typecheck   # dashboard type check
cd web && pnpm build       # dashboard production build
make test           # vet + go test + pnpm typecheck
make demo           # build the dashboard and run the demo on :8080 (demo / demo, token demo-token)
```

Dashboard development with hot reload: run the server on port 8090 (the Vite dev proxy target), then the Vite dev server:

```bash
go run ./cmd/hiwikinsight demo -data ./demo-data -listen :8090
cd web && pnpm dev
```

Go code builds without the dashboard; `/dashboard/` then answers 503 "Dashboard not built".

## Conventions

- **Go stdlib first.** Current dependencies: `modernc.org/sqlite`, `gopkg.in/yaml.v3`, `golang.org/x/crypto` (bcrypt), `golang.org/x/term`. Add a dependency only with a strong reason. `cmd/insight` stays stdlib-only so `go install` is light.
- **Pure-Go SQLite** (`modernc.org/sqlite`), `CGO_ENABLED=0` everywhere. No cgo.
- **Days are in the reporting time zone.** Every `day`, cohort, report period and alert window uses `tz` (`tz.Day`, `tz.Today`, `tz.ParseDay`, `tz.StartOfDay`) in Go and `local_date(ms)`, `local_datetime(ms)`, `local_today()` in SQL. Never hard-code a UTC offset or use `date(ts/1000, 'unixepoch')`. App Store Connect report periods are the exception: they are Pacific Time days, as Apple defines them.
- **Money is in the base currency** (`currency` in the config). `appstore_events.amount` and `asc_sales.proceeds` are already converted; do not reconvert. Free trials (`price_milli = 0`) are not sales.
- **Server-generated text** (emails, reports, alerts) goes through `i18n.T("English", "中文")`, English first.
- **Dashboard strings** go through `t('中文', 'English')` from `web/src/prefs.tsx`, Chinese first.
- **Colors** in the dashboard only come from CSS variables in `web/src/styles/tokens.css` (light and dark). Never hard-code a color in a component. Charts read tokens through `usePalette()`; a token used by a chart must also be listed in `tokenNames` in `web/src/theme.tsx`.
- **Migrations are append-only.** Add a new string to the end of `migrations` in `internal/store/migrations.go`. Never edit or reorder an existing one; deployed databases track progress with `PRAGMA user_version`. Views must use `json_extract`, not `->>`, so older `sqlite3` CLIs can still open the database.
- **The query API stays read-only.** It runs on `Store.ReadOnly()` (`mode=ro`, `query_only`), and `query.ValidateSQL` allows one `SELECT`/`WITH`. Do not add write endpoints under `/v1/query/`.
- **The event wire protocol stays backward compatible** with HiwiKInsightKit ([PROTOCOL.md](https://github.com/RedHiwiK/hiwikinsight-ios/blob/main/PROTOCOL.md)). Schema 1 must keep working; change PROTOCOL.md first.
- **Privacy.** Ingestion never reads client IPs. Do not add fields that identify a person.
- **Metric definitions are a contract.** Changing what a metric counts changes the `Definition` string too; AI agents and users rely on it.
- **Commits**: Conventional Commits in English (`feat(metrics): ...`, `fix(ingest): ...`).

## How to add a metric

1. Write the computation in `internal/metrics/` (a new file or next to related metrics): `func myMetric(ctx context.Context, db *sql.DB, p Params) ([]Table, error)`. Use `p.App`, `p.Env`, `p.From`, `p.To`, `p.Bundle` and `p.arg("name")` for extra parameters. Use `day` columns and `local_date()`; validate any parameter that ends up in SQL text.
2. Register it with a precise one-sentence-or-more definition: append `Def{"my_metric", "Definition ...", myMetric}` to `defs` (in an `init()` like `dashboard.go` does, or in the `defs` literal in `metrics.go`).
3. Add a test with seeded data (see `internal/metrics/*_test.go`).
4. It is now available at `GET /v1/query/metric/my_metric`, `insight metric my_metric`, the MCP `metric` tool, and listed by `describe`. Document it in `docs/metrics.md` and, if agents should know it by name, in `skills/hiwikinsight-analytics/SKILL.md` and the `metric` tool description in `cmd/insight/mcp.go`.

## How to add a dashboard page

1. Create `web/src/pages/MyPage.tsx`; follow an existing page (for example `Errors.tsx`): `Page` wrapper, data through `api` / React Query, strings through `t('中文', 'English')`, colors through tokens.
2. Add a route in `web/src/App.tsx` and a nav item in `web/src/components/Layout.tsx`.
3. If it needs data no metric provides, add a metric first (above) rather than a new endpoint.
4. New colors: add the variable to both themes in `web/src/styles/tokens.css`; if a chart uses it, add it to `tokenNames` in `web/src/theme.tsx`.
5. `cd web && pnpm typecheck && pnpm build`, then check it in the demo in light and dark mode and in both languages.

## How to add an alert rule

1. In `internal/report/alert.go`, write `func myAlerts(ctx context.Context, db *sql.DB, rules AlertRules, app App, dayStart int64, now time.Time) ([]alert, error)` returning `alert{rule: "my_rule", key: ..., view: alertView(...)}`. `rule` + `key` deduplicate: each is sent at most once per day.
2. Add it to the per-app check list in `checkAlerts` (or call it once, like `ascStaleAlert`, if it is global).
3. If it needs a threshold: add it to `AlertRules` (`internal/report/report.go`), to `Alerts` in `internal/config/config.go` with a default in `applyDefaults`, and map it in `internal/server/server.go` where `report.AlertRules` is built.
4. All text through `i18n.T`. Production data only (`env = 'production'`).
5. Add a test in `internal/report/alert_test.go` and document the rule in `docs/reports-and-alerts.md`.

## How to add a config field

1. Add the field with a `yaml` tag and a doc comment to the right struct in `internal/config/config.go`.
2. Default in `applyDefaults`, validation in `validate`. Paths must go through `c.Resolve` in `Load`.
3. Use it where needed (usually wired in `internal/server/server.go`).
4. Add it to `examples/config.example.yaml` and `docs/configuration.md`; add a case to `internal/config/config_test.go`.
5. Unknown YAML keys are rejected (`KnownFields(true)`), so a renamed field breaks existing configs; avoid renames.

## How to verify a change

```bash
go vet ./... && go test ./...
cd web && pnpm typecheck && cd ..
make build && ./bin/hiwikinsight demo -data ./demo-data
```

Then, against the demo:

```bash
curl -s localhost:8080/healthz
curl -s -H "Authorization: Bearer demo-token" localhost:8080/v1/query/describe | head -60
curl -s -H "Authorization: Bearer demo-token" "localhost:8080/v1/query/metric/active_users?app=pawprint&from=2026-09-01&to=2026-09-30"
curl -s -H "Authorization: Bearer demo-token" -d '{"sql":"SELECT name, COUNT(*) FROM events GROUP BY name ORDER BY 2 DESC"}' localhost:8080/v1/query/sql
./bin/insight --token demo-token metric retention --app pawprint
```

Reports render without sending mail: sign in to the dashboard, then open `/v1/admin/report/preview?kind=daily` (or `kind=weekly`, `format=text`). The demo database is reused between runs; delete `./demo-data` to regenerate it with current dates.

## Using HiwiKInsight data from an agent

Not the same task as changing this repository. See [docs/ai-integration.md](docs/ai-integration.md): run `insight describe` (or call the MCP `describe` tool) first, prefer predefined metrics, and use read-only SQL only when no metric fits.
