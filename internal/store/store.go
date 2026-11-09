// Package store is the only persistence layer of HiwiKInsight (SQLite):
// the App Store transaction log, first-party analytics events, and the install / daily active /
// purchase attribution tables maintained from those events.
package store

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed device_models.csv
var deviceModelsCSV string

type Store struct {
	db *sql.DB // read-write, single connection: SQLite allows only one writer
	ro *sql.DB // read-only pool for the query API and stats; does not block writes under WAL
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if err := loadDeviceModels(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: device models: %w", err)
	}
	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		db.Close()
		return nil, err
	}
	ro.SetMaxOpenConns(4)
	return &Store{db: db, ro: ro}, nil
}

// ReadOnly returns the read-only connection pool (mode=ro + query_only) used by the query API to run arbitrary SELECTs.
func (s *Store) ReadOnly() *sql.DB { return s.ro }

func (s *Store) Close() error {
	s.ro.Close()
	return s.db.Close()
}

// The device model mapping ships with the binary and replaces the whole table on every start.
func loadDeviceModels(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, line := range strings.Split(strings.TrimSpace(deviceModelsCSV), "\n") {
		if i == 0 {
			continue // header
		}
		// The identifier itself contains a comma (iPhone17,1) and the name does not, so split on the last comma
		idx := strings.LastIndex(line, ",")
		if idx <= 0 {
			continue
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO device_models (identifier, name) VALUES (?, ?)`,
			strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:])); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type Event struct {
	NotificationUUID string
	BundleID         string
	Environment      string
	NotificationType string
	Subtype          string
	ProductID        string
	TransactionID    string
	Storefront       string
	Currency         string
	PriceMilli       int64
	RevenueSign      int
	Amount           *float64 // price converted to the base currency; nil when no exchange rate
	SignedAt         int64
	AppAccountToken  string // appAccountToken passed by the app at purchase time, lowercased on write
	// Used by v_attribution to pick the purchase attempt when an app reuses one token for many purchases.
	App                   string // app key from the config
	OriginalTransactionID string
	TransactionReason     string // PURCHASE (user-initiated) / RENEWAL
	PurchaseAt            int64  // transaction purchaseDate (milliseconds); 0 when the notification has no transaction
	OriginalPurchaseAt    int64
}

// Record writes idempotently by notificationUUID; returns inserted=false when Apple retries the same notification.
func (s *Store) Record(e Event) (inserted bool, err error) {
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO appstore_events
		(notification_uuid, bundle_id, environment, notification_type, subtype, product_id, transaction_id,
		 storefront, currency, price_milli, revenue_sign, amount, signed_at, received_at, app_account_token,
		 app, original_transaction_id, transaction_reason, purchase_at, original_purchase_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.NotificationUUID, e.BundleID, e.Environment, e.NotificationType, e.Subtype, e.ProductID, e.TransactionID,
		e.Storefront, e.Currency, e.PriceMilli, e.RevenueSign, e.Amount, e.SignedAt, time.Now().UnixMilli(),
		strings.ToLower(e.AppAccountToken),
		e.App, e.OriginalTransactionID, e.TransactionReason, nullMillis(e.PurchaseAt), nullMillis(e.OriginalPurchaseAt))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// FreeTrialSQL matches a free-trial start: a subscription that began at price 0. Only subscriptions have
// trials; any other price-0 income row (an offer code redeemed for a non-consumable, a free renewal period)
// is free but neither a trial nor a sale. IsFreeTrial is the same rule in Go.
const FreeTrialSQL = `(revenue_sign = 1 AND notification_type = 'SUBSCRIBED' AND COALESCE(price_milli, 0) = 0)`

func IsFreeTrial(notificationType string, priceMilli int64) bool {
	return notificationType == "SUBSCRIBED" && priceMilli == 0
}

// TrialConversionSubtype marks the first charge after a free trial, which Apple reports
// as a plain DID_RENEW. It is not an Apple value: SubtypeSQL derives it when reading.
const TrialConversionSubtype = "TRIAL_CONVERSION"

// SubtypeSQL selects the subtype of an appstore_events row aliased as a.
const SubtypeSQL = `CASE WHEN a.notification_type = 'DID_RENEW' AND a.original_transaction_id != ''
		AND EXISTS (SELECT 1 FROM appstore_events t WHERE t.original_transaction_id = a.original_transaction_id
			AND ` + FreeTrialSQL + `)
		AND NOT EXISTS (SELECT 1 FROM appstore_events p WHERE p.original_transaction_id = a.original_transaction_id
			AND p.revenue_sign = 1 AND p.price_milli > 0 AND p.signed_at < a.signed_at)
		THEN '` + TrialConversionSubtype + `' ELSE a.subtype END`

// IsTrialConversion reports whether a stored notification is the first charge after a free trial.
func (s *Store) IsTrialConversion(notificationUUID string) (bool, error) {
	var subtype string
	err := s.db.QueryRow(`SELECT `+SubtypeSQL+` FROM appstore_events a WHERE a.notification_uuid = ?`, notificationUUID).Scan(&subtype)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return subtype == TrialConversionSubtype, err
}

type Period struct {
	Sales       int     // number of paid transactions (price > 0; free-trial starts excluded)
	Trials      int     // free-trial starts (FreeTrialSQL)
	Refunds     int     // number of refunds
	Net         float64 // sales minus refunds, base currency
	Unconverted int     // transactions left out of Net because no exchange rate was available
}

type Stats struct {
	Today, Month Period
}

// Stats returns revenue of one app and environment for the calendar day / calendar month containing now (in loc).
func (s *Store) Stats(bundleID, environment string, now time.Time, loc *time.Location) (Stats, error) {
	now = now.In(loc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)

	var st Stats
	for _, p := range []struct {
		from int64
		out  *Period
	}{{dayStart.UnixMilli(), &st.Today}, {monthStart.UnixMilli(), &st.Month}} {
		err := s.db.QueryRow(`
			SELECT
				COALESCE(SUM(revenue_sign = 1 AND price_milli > 0), 0),
				COALESCE(SUM(`+FreeTrialSQL+`), 0),
				COALESCE(SUM(revenue_sign = -1), 0),
				COALESCE(SUM(revenue_sign * amount), 0),
				COALESCE(SUM(revenue_sign != 0 AND COALESCE(price_milli, 0) != 0 AND amount IS NULL), 0)
			FROM appstore_events
			WHERE bundle_id = ? AND environment = ? AND signed_at >= ?`,
			bundleID, environment, p.from,
		).Scan(&p.out.Sales, &p.out.Trials, &p.out.Refunds, &p.out.Net, &p.out.Unconverted)
		if err != nil {
			return Stats{}, err
		}
	}
	return st, nil
}

// nullMillis stores an unknown time as NULL rather than 0, so time comparisons in views skip it.
func nullMillis(ms int64) any {
	if ms <= 0 {
		return nil
	}
	return ms
}
