package store

import (
	"database/sql"
	"fmt"
)

// Migrations run in order according to PRAGMA user_version; append only, never modify.
// v1 matches the original appstore_events table (early databases had user_version 0 with the table
// already present, hence IF NOT EXISTS).
var migrations = []string{
	// v1: App Store transaction log
	`
CREATE TABLE IF NOT EXISTS appstore_events (
	notification_uuid TEXT PRIMARY KEY,
	bundle_id         TEXT NOT NULL,
	environment       TEXT NOT NULL,
	notification_type TEXT NOT NULL,
	subtype           TEXT NOT NULL DEFAULT '',
	product_id        TEXT NOT NULL DEFAULT '',
	transaction_id    TEXT NOT NULL DEFAULT '',
	storefront        TEXT NOT NULL DEFAULT '',
	currency          TEXT NOT NULL DEFAULT '',
	price_milli       INTEGER NOT NULL DEFAULT 0,
	revenue_sign      INTEGER NOT NULL DEFAULT 0, -- +1 revenue / -1 refund / 0 non-monetary event
	cny               REAL,                       -- converted amount in the base currency, NULL when no exchange rate is available
	signed_at         INTEGER NOT NULL,           -- Apple signing time (milliseconds)
	received_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_appstore_events_stats
	ON appstore_events (bundle_id, environment, signed_at);
`,
	// v2: first-party analytics
	`
ALTER TABLE appstore_events ADD COLUMN app_account_token TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_appstore_events_token ON appstore_events (app_account_token) WHERE app_account_token != '';

-- Raw events, kept for 365 days
CREATE TABLE events (
	event_id    TEXT PRIMARY KEY,
	app         TEXT NOT NULL,
	env         TEXT NOT NULL,
	install_id  TEXT NOT NULL,
	session_id  TEXT,
	name        TEXT NOT NULL,
	ts          INTEGER NOT NULL,  -- corrected timestamp in milliseconds
	day         TEXT NOT NULL,     -- date of ts in the reporting time zone, YYYY-MM-DD
	received_at INTEGER NOT NULL,
	app_version TEXT, build TEXT, os TEXT, os_version TEXT, device TEXT,
	locale TEXT, language TEXT, region TEXT, storefront TEXT, appearance TEXT, text_size TEXT,
	params      TEXT NOT NULL DEFAULT '{}'
) WITHOUT ROWID;
CREATE INDEX idx_events_app_name_ts ON events (app, env, name, ts);
CREATE INDEX idx_events_app_ts ON events (app, env, ts);
CREATE INDEX idx_events_install ON events (app, install_id, ts);

-- One row per install, kept forever
CREATE TABLE installs (
	app           TEXT NOT NULL,
	install_id    TEXT NOT NULL,
	env           TEXT NOT NULL,
	first_seen    INTEGER NOT NULL,  -- time of the first event seen by the server (milliseconds)
	acquired_at   INTEGER NOT NULL,  -- min(first_seen, AppTransaction original download time)
	first_version TEXT,
	last_seen     INTEGER NOT NULL,
	last_version  TEXT, device TEXT, os_version TEXT, region TEXT, storefront TEXT, language TEXT,
	PRIMARY KEY (app, install_id)
) WITHOUT ROWID;

-- Per user x day activity, kept forever; DAU / retention / resurrection are all computed from it
CREATE TABLE daily_active (
	app        TEXT NOT NULL,
	env        TEXT NOT NULL,
	day        TEXT NOT NULL,
	install_id TEXT NOT NULL,
	sessions   INTEGER NOT NULL DEFAULT 0,
	duration_s INTEGER NOT NULL DEFAULT 0,
	events     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (app, env, day, install_id)
) WITHOUT ROWID;
CREATE INDEX idx_daily_active_install ON daily_active (app, install_id, day);

-- Purchase attribution: the token of purchase.started is the StoreKit appAccountToken
CREATE TABLE purchase_attempts (
	token       TEXT PRIMARY KEY,
	app         TEXT NOT NULL,
	env         TEXT NOT NULL,
	install_id  TEXT NOT NULL,
	context     TEXT NOT NULL DEFAULT '',
	product     TEXT NOT NULL DEFAULT '',
	ts          INTEGER NOT NULL,
	app_version TEXT, device TEXT, os_version TEXT
) WITHOUT ROWID;

CREATE TABLE device_models (
	identifier TEXT PRIMARY KEY,
	name       TEXT NOT NULL
) WITHOUT ROWID;

-- Analysis views for AI assistants and the dashboard; day is always a date in the reporting time zone
CREATE VIEW v_user_days AS
SELECT app, env, day, install_id, sessions, duration_s, events FROM daily_active;

CREATE VIEW v_user_summary AS
SELECT
	i.app, i.env, i.install_id,
	date(i.first_seen / 1000 + 28800, 'unixepoch')  AS first_seen_day,
	date(i.acquired_at / 1000 + 28800, 'unixepoch') AS acquired_day,
	date(i.first_seen / 1000 + 28800, 'unixepoch') = date(i.acquired_at / 1000 + 28800, 'unixepoch') AS is_new,
	date(i.last_seen / 1000 + 28800, 'unixepoch')   AS last_seen_day,
	i.first_version, i.last_version,
	i.device, COALESCE(m.name, i.device) AS device_name,
	i.os_version, i.region, i.storefront, i.language,
	(SELECT COUNT(*) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS active_days,
	(SELECT COALESCE(SUM(sessions), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS sessions,
	(SELECT COALESCE(SUM(duration_s), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS duration_s,
	EXISTS (
		SELECT 1 FROM purchase_attempts p JOIN appstore_events a ON a.app_account_token = p.token
		WHERE p.app = i.app AND p.install_id = i.install_id AND a.revenue_sign = 1
	) AS is_paid
FROM installs i LEFT JOIN device_models m ON m.identifier = i.device;

CREATE VIEW v_sessions AS
SELECT
	app, env, install_id, session_id,
	MIN(ts) AS started_at,
	MIN(day) AS day,
	MAX(CASE WHEN name = 'session.started' THEN params ->> '$.source' END) AS source,
	MAX(CASE WHEN name = 'session.ended' THEN CAST(params ->> '$.duration_s' AS INTEGER) END) AS duration_s,
	SUM(name = 'screen.viewed') AS screen_views,
	COUNT(*) AS events
FROM events
WHERE session_id IS NOT NULL AND session_id != ''
GROUP BY app, env, install_id, session_id;

CREATE VIEW v_screen_usage AS
SELECT
	app, env, day, install_id, session_id, ts,
	CASE name WHEN 'screen.viewed' THEN 'viewed' ELSE 'left' END AS kind,
	params ->> '$.screen' AS screen,
	params ->> '$.module' AS module,
	CAST(params ->> '$.duration_s' AS INTEGER) AS duration_s
FROM events
WHERE name IN ('screen.viewed', 'screen.left');
`,
	// v3: views switch to json_extract. Older sqlite3 command-line tools (< 3.38) do not understand ->>,
	// and a view containing it makes the whole database unusable from the CLI (including .backup);
	// the SQLite embedded in Go is not affected
	`
DROP VIEW v_sessions;
DROP VIEW v_screen_usage;

CREATE VIEW v_sessions AS
SELECT
	app, env, install_id, session_id,
	MIN(ts) AS started_at,
	MIN(day) AS day,
	MAX(CASE WHEN name = 'session.started' THEN json_extract(params, '$.source') END) AS source,
	MAX(CASE WHEN name = 'session.ended' THEN CAST(json_extract(params, '$.duration_s') AS INTEGER) END) AS duration_s,
	SUM(name = 'screen.viewed') AS screen_views,
	COUNT(*) AS events
FROM events
WHERE session_id IS NOT NULL AND session_id != ''
GROUP BY app, env, install_id, session_id;

CREATE VIEW v_screen_usage AS
SELECT
	app, env, day, install_id, session_id, ts,
	CASE name WHEN 'screen.viewed' THEN 'viewed' ELSE 'left' END AS kind,
	json_extract(params, '$.screen') AS screen,
	json_extract(params, '$.module') AS module,
	CAST(json_extract(params, '$.duration_s') AS INTEGER) AS duration_s
FROM events
WHERE name IN ('screen.viewed', 'screen.left');
`,
	// v4: App Store Connect sales reports (historical revenue and downloads for the portfolio dashboard)
	`
CREATE TABLE asc_apps (
	apple_id   TEXT PRIMARY KEY,
	bundle_id  TEXT NOT NULL,
	sku        TEXT NOT NULL,
	name       TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);

-- Report fetch log: ok = stored; empty = confirmed no sales for the period (Apple returned 404 after the report should have been available)
CREATE TABLE asc_reports (
	frequency  TEXT NOT NULL,    -- DAILY / MONTHLY
	period     TEXT NOT NULL,    -- YYYY-MM-DD / YYYY-MM (Pacific Time, same as Apple's reports)
	status     TEXT NOT NULL,
	row_count  INTEGER NOT NULL,
	fetched_at INTEGER NOT NULL,
	PRIMARY KEY (frequency, period)
);

-- Summary Sales Report rows; a period is replaced as a whole, so imports are idempotent
CREATE TABLE asc_sales (
	frequency         TEXT NOT NULL,
	period            TEXT NOT NULL,
	month             TEXT NOT NULL,           -- YYYY-MM
	bundle_id         TEXT NOT NULL DEFAULT '', -- in-app purchase rows are mapped to their app via Parent Identifier
	sku               TEXT NOT NULL,
	parent_sku        TEXT NOT NULL DEFAULT '',
	title             TEXT NOT NULL DEFAULT '',
	product_type      TEXT NOT NULL,
	category          TEXT NOT NULL,           -- download / redownload / update / iap / subscription / other
	units             REAL NOT NULL,           -- negative for refunds
	proceeds_per_unit REAL NOT NULL,           -- proceeds per unit (after commission and taxes), in the original currency
	proceeds_currency TEXT NOT NULL DEFAULT '',
	proceeds_cny      REAL,                    -- units x proceeds per unit, converted to the base currency at fetch time; NULL when no exchange rate is available
	customer_price    REAL NOT NULL DEFAULT 0,
	customer_currency TEXT NOT NULL DEFAULT '',
	country           TEXT NOT NULL DEFAULT '',
	apple_id          TEXT NOT NULL DEFAULT '',
	subscription      TEXT NOT NULL DEFAULT '', -- New / Renewal
	sub_period        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_asc_sales_period ON asc_sales (frequency, period);
CREATE INDEX idx_asc_sales_month ON asc_sales (month, bundle_id);
`,
	// v5: prior_usage from SDK 0.1.2. Installs stored before the server supported it are backfilled once
	// from raw events (same rule as WriteBatches)
	`
UPDATE installs SET acquired_at = MIN(acquired_at, first_seen - 86400000)
WHERE EXISTS (
	SELECT 1 FROM events e
	WHERE e.app = installs.app AND e.install_id = installs.install_id AND e.name = 'app.installed'
		AND json_extract(e.params, '$.prior_usage') = '1'
		AND json_extract(e.params, '$.original_download_ts') IS NULL
);
`,
	// v6: App Store Connect analytics reports (store impressions → product page → first-time downloads, by source and country)
	`
-- Processed report instances; processed ones are not downloaded again
CREATE TABLE asc_analytics_instances (
	instance_id     TEXT PRIMARY KEY,
	report          TEXT NOT NULL,  -- report name, e.g. App Store Discovery and Engagement Standard
	bundle_id       TEXT NOT NULL,
	processing_date TEXT NOT NULL,
	row_count       INTEGER NOT NULL,
	fetched_at      INTEGER NOT NULL
);

-- Impressions and product page views; each instance is a full snapshot of the last few days, and
-- (bundle_id, date) is replaced as a whole by an instance with a later processing date
CREATE TABLE asc_store_engagement (
	date               TEXT NOT NULL,   -- YYYY-MM-DD (Apple report date)
	bundle_id          TEXT NOT NULL,
	source             TEXT NOT NULL,   -- search / browse / app_referrer / web_referrer / other
	territory          TEXT NOT NULL,
	impressions        INTEGER NOT NULL,
	impressions_unique INTEGER NOT NULL, -- summed across sources and countries, the same device may be counted more than once
	page_views         INTEGER NOT NULL,
	page_views_unique  INTEGER NOT NULL,
	PRIMARY KEY (date, bundle_id, source, territory)
);

-- Downloads (analytics report definition, with source); same replacement rule as above
CREATE TABLE asc_store_downloads (
	date            TEXT NOT NULL,
	bundle_id       TEXT NOT NULL,
	source          TEXT NOT NULL,
	territory       TEXT NOT NULL,
	first_downloads INTEGER NOT NULL,
	redownloads     INTEGER NOT NULL,
	PRIMARY KEY (date, bundle_id, source, territory)
);
`,
	// v7: daily / weekly report send log, so reports are not resent after a restart
	`
CREATE TABLE report_sends (
	kind    TEXT NOT NULL,    -- daily / weekly
	period  TEXT NOT NULL,    -- the reported day for daily reports, the Monday of the reported week for weekly reports (YYYY-MM-DD, reporting time zone)
	sent_at INTEGER NOT NULL,
	PRIMARY KEY (kind, period)
);
`,
	// v8: open-source release.
	//   - Revenue is stored in the configured base currency, so columns no longer say "cny".
	//   - Day boundaries follow the configured time zone through the local_date() SQL
	//     function (package tz) instead of a hard-coded UTC+8 offset.
	`
ALTER TABLE appstore_events RENAME COLUMN cny TO amount;
ALTER TABLE asc_sales RENAME COLUMN proceeds_cny TO proceeds;

DROP VIEW v_user_summary;
CREATE VIEW v_user_summary AS
SELECT
	i.app, i.env, i.install_id,
	local_date(i.first_seen)  AS first_seen_day,
	local_date(i.acquired_at) AS acquired_day,
	local_date(i.first_seen) = local_date(i.acquired_at) AS is_new,
	local_date(i.last_seen)   AS last_seen_day,
	i.first_version, i.last_version,
	i.device, COALESCE(m.name, i.device) AS device_name,
	i.os_version, i.region, i.storefront, i.language,
	(SELECT COUNT(*) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS active_days,
	(SELECT COALESCE(SUM(sessions), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS sessions,
	(SELECT COALESCE(SUM(duration_s), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS duration_s,
	EXISTS (
		SELECT 1 FROM purchase_attempts p JOIN appstore_events a ON a.app_account_token = p.token
		WHERE p.app = i.app AND p.install_id = i.install_id AND a.revenue_sign = 1
	) AS is_paid
FROM installs i LEFT JOIN device_models m ON m.identifier = i.device;
`,
	// v9: is_paid counts real sales only; a free-trial start (revenue_sign = 1 at price 0) is not a sale.
	`
DROP VIEW v_user_summary;
CREATE VIEW v_user_summary AS
SELECT
	i.app, i.env, i.install_id,
	local_date(i.first_seen)  AS first_seen_day,
	local_date(i.acquired_at) AS acquired_day,
	local_date(i.first_seen) = local_date(i.acquired_at) AS is_new,
	local_date(i.last_seen)   AS last_seen_day,
	i.first_version, i.last_version,
	i.device, COALESCE(m.name, i.device) AS device_name,
	i.os_version, i.region, i.storefront, i.language,
	(SELECT COUNT(*) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS active_days,
	(SELECT COALESCE(SUM(sessions), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS sessions,
	(SELECT COALESCE(SUM(duration_s), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS duration_s,
	EXISTS (
		SELECT 1 FROM purchase_attempts p JOIN appstore_events a ON a.app_account_token = p.token
		WHERE p.app = i.app AND p.install_id = i.install_id AND a.revenue_sign = 1 AND a.price_milli > 0
	) AS is_paid
FROM installs i LEFT JOIN device_models m ON m.identifier = i.device;
`,
	// v10: is_pro is the user's current membership as last reported by the app: is_pro in the latest
	// user.snapshot, or a purchase or trial start confirmed by the App Store after that snapshot and not
	// followed by an expiry, refund or revocation of the same appAccountToken (renewals keep the token, and
	// a lapsed user may never open the app again to send a new snapshot). Unlike is_paid it covers members
	// who bought before the SDK was integrated or restored a purchase.
	// The snapshot lookups walk idx_events_install backwards and stop at the first match.
	`
DROP VIEW v_user_summary;
CREATE VIEW v_user_summary AS
SELECT
	i.app, i.env, i.install_id,
	local_date(i.first_seen)  AS first_seen_day,
	local_date(i.acquired_at) AS acquired_day,
	local_date(i.first_seen) = local_date(i.acquired_at) AS is_new,
	local_date(i.last_seen)   AS last_seen_day,
	i.first_version, i.last_version,
	i.device, COALESCE(m.name, i.device) AS device_name,
	i.os_version, i.region, i.storefront, i.language,
	(SELECT COUNT(*) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS active_days,
	(SELECT COALESCE(SUM(sessions), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS sessions,
	(SELECT COALESCE(SUM(duration_s), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS duration_s,
	EXISTS (
		SELECT 1 FROM purchase_attempts p JOIN appstore_events a ON a.app_account_token = p.token
		WHERE p.app = i.app AND p.install_id = i.install_id AND a.revenue_sign = 1 AND a.price_milli > 0
	) AS is_paid,
	(
		COALESCE((
			SELECT json_extract(e.params, '$.is_pro') IN ('true', '1') FROM events e
			WHERE e.app = i.app AND e.install_id = i.install_id AND e.name = 'user.snapshot'
			ORDER BY e.ts DESC LIMIT 1
		), 0)
		OR EXISTS (
			SELECT 1 FROM purchase_attempts p JOIN appstore_events a ON a.app_account_token = p.token
			WHERE p.app = i.app AND p.install_id = i.install_id AND a.revenue_sign = 1
				AND a.signed_at > COALESCE((
					SELECT e.ts FROM events e
					WHERE e.app = i.app AND e.install_id = i.install_id AND e.name = 'user.snapshot'
					ORDER BY e.ts DESC LIMIT 1
				), 0)
				AND NOT EXISTS (
					SELECT 1 FROM appstore_events x
					WHERE x.app_account_token = p.token AND x.signed_at > a.signed_at
						AND (x.revenue_sign = -1 OR x.notification_type IN ('EXPIRED', 'GRACE_PERIOD_EXPIRED', 'REVOKE'))
				)
		)
	) AS is_pro
FROM installs i LEFT JOIN device_models m ON m.identifier = i.device;
`,
	// v11: an app may pass its own stable appAccountToken (an account ID) on every purchase, so one token
	// can have many purchase attempts. purchase_attempts gets a surrogate key (retried batches are still
	// deduplicated by token + install + time), appstore_events keeps the fields needed to pick the right
	// attempt, and v_attribution is the single place that maps a notification to one attempt. Tokens
	// generated per purchase have one attempt each and resolve exactly as before.
	`
ALTER TABLE appstore_events ADD COLUMN app TEXT NOT NULL DEFAULT '';
ALTER TABLE appstore_events ADD COLUMN original_transaction_id TEXT NOT NULL DEFAULT '';
ALTER TABLE appstore_events ADD COLUMN transaction_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE appstore_events ADD COLUMN purchase_at INTEGER;
ALTER TABLE appstore_events ADD COLUMN original_purchase_at INTEGER;
CREATE INDEX idx_appstore_events_original ON appstore_events (original_transaction_id, purchase_at)
	WHERE original_transaction_id != '';

DROP VIEW v_user_summary;
CREATE TABLE purchase_attempts_v11 (
	id          INTEGER PRIMARY KEY,
	token       TEXT NOT NULL,
	app         TEXT NOT NULL,
	env         TEXT NOT NULL,
	install_id  TEXT NOT NULL,
	context     TEXT NOT NULL DEFAULT '',
	product     TEXT NOT NULL DEFAULT '',
	ts          INTEGER NOT NULL,
	app_version TEXT, device TEXT, os_version TEXT,
	UNIQUE (token, install_id, ts)
);
INSERT INTO purchase_attempts_v11 (token, app, env, install_id, context, product, ts, app_version, device, os_version)
SELECT token, app, env, install_id, context, product, ts, app_version, device, os_version
FROM purchase_attempts ORDER BY ts;
DROP TABLE purchase_attempts;
ALTER TABLE purchase_attempts_v11 RENAME TO purchase_attempts;
CREATE INDEX idx_purchase_attempts_token_ts ON purchase_attempts (token, ts);
CREATE INDEX idx_purchase_attempts_install ON purchase_attempts (app, install_id);

-- One row per notification that carries an appAccountToken; attempt_id is NULL when nothing matches.
-- The attempt is the latest one with the same token in the same app started no later than 5 minutes after
-- the moment the user bought: the transaction's own purchase time for a user-initiated purchase
-- (transactionReason PURCHASE); otherwise (renewals, refunds, expiry) the purchase time of the latest stored
-- user-initiated transaction with the same originalTransactionId, or else the original purchase time.
-- Resubscribing keeps the originalTransactionId but is a new PURCHASE, so it is credited to its own attempt.
-- When nothing qualifies, the latest attempt before the transaction itself is used; rows stored before v11
-- have no purchase times and match any attempt of the token, as before.
CREATE VIEW v_attribution AS
SELECT
	a.notification_uuid,
	COALESCE(
		(
			SELECT p.id FROM purchase_attempts p
			WHERE p.token = a.app_account_token AND (a.app = '' OR p.app = a.app)
				AND p.ts <= 300000 + CASE WHEN a.transaction_reason = 'PURCHASE' THEN a.purchase_at ELSE COALESCE((
					SELECT s.purchase_at FROM appstore_events s
					WHERE s.original_transaction_id = a.original_transaction_id AND a.original_transaction_id != ''
						AND s.transaction_reason = 'PURCHASE' AND s.purchase_at <= a.purchase_at
					ORDER BY s.purchase_at DESC LIMIT 1
				), a.original_purchase_at) END
			ORDER BY p.ts DESC, p.id DESC LIMIT 1
		),
		(
			SELECT p.id FROM purchase_attempts p
			WHERE p.token = a.app_account_token AND (a.app = '' OR p.app = a.app)
				AND (a.purchase_at IS NULL OR p.ts <= a.purchase_at + 300000)
			ORDER BY p.ts DESC, p.id DESC LIMIT 1
		)
	) AS attempt_id
FROM appstore_events a
WHERE a.app_account_token != '';

CREATE VIEW v_user_summary AS
SELECT
	i.app, i.env, i.install_id,
	local_date(i.first_seen)  AS first_seen_day,
	local_date(i.acquired_at) AS acquired_day,
	local_date(i.first_seen) = local_date(i.acquired_at) AS is_new,
	local_date(i.last_seen)   AS last_seen_day,
	i.first_version, i.last_version,
	i.device, COALESCE(m.name, i.device) AS device_name,
	i.os_version, i.region, i.storefront, i.language,
	(SELECT COUNT(*) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS active_days,
	(SELECT COALESCE(SUM(sessions), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS sessions,
	(SELECT COALESCE(SUM(duration_s), 0) FROM daily_active d WHERE d.app = i.app AND d.install_id = i.install_id) AS duration_s,
	EXISTS (
		SELECT 1 FROM purchase_attempts p
		JOIN appstore_events a ON a.app_account_token = p.token
		JOIN v_attribution v ON v.notification_uuid = a.notification_uuid AND v.attempt_id = p.id
		WHERE p.app = i.app AND p.install_id = i.install_id AND a.revenue_sign = 1 AND a.price_milli > 0
	) AS is_paid,
	(
		COALESCE((
			SELECT json_extract(e.params, '$.is_pro') IN ('true', '1') FROM events e
			WHERE e.app = i.app AND e.install_id = i.install_id AND e.name = 'user.snapshot'
			ORDER BY e.ts DESC LIMIT 1
		), 0)
		OR EXISTS (
			SELECT 1 FROM purchase_attempts p
			JOIN appstore_events a ON a.app_account_token = p.token
			JOIN v_attribution v ON v.notification_uuid = a.notification_uuid AND v.attempt_id = p.id
			WHERE p.app = i.app AND p.install_id = i.install_id AND a.revenue_sign = 1
				AND a.signed_at > COALESCE((
					SELECT e.ts FROM events e
					WHERE e.app = i.app AND e.install_id = i.install_id AND e.name = 'user.snapshot'
					ORDER BY e.ts DESC LIMIT 1
				), 0)
				AND NOT EXISTS (
					SELECT 1 FROM appstore_events x
					JOIN v_attribution xv ON xv.notification_uuid = x.notification_uuid AND xv.attempt_id = p.id
					WHERE x.app_account_token = p.token AND x.signed_at > a.signed_at
						AND (x.revenue_sign = -1 OR x.notification_type IN ('EXPIRED', 'GRACE_PERIOD_EXPIRED', 'REVOKE'))
				)
		)
	) AS is_pro
FROM installs i LEFT JOIN device_models m ON m.identifier = i.device;
`,
	// v12: keep the offer a transaction used, so a price-0 offer-code redemption is not taken for a free trial.
	`
ALTER TABLE appstore_events ADD COLUMN offer_type INTEGER NOT NULL DEFAULT 0;
ALTER TABLE appstore_events ADD COLUMN offer_identifier TEXT NOT NULL DEFAULT '';
`,
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration v%d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
