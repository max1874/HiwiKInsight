// Package notify receives App Store Server Notifications V2, records them as
// transactions and sends a transaction email for each one, plus a chat message for
// charges and refunds when a chat is configured.
package notify

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/appstore"
	"github.com/RedHiwiK/HiwiKInsight/internal/config"
	"github.com/RedHiwiK/HiwiKInsight/internal/fx"
	"github.com/RedHiwiK/HiwiKInsight/internal/mailer"
	"github.com/RedHiwiK/HiwiKInsight/internal/store"
	"github.com/RedHiwiK/HiwiKInsight/internal/tz"
)

// Handler serves POST /v1/appstore/notifications.
type Handler struct {
	Config   *config.Config
	Verifier *appstore.Verifier
	Sender   mailer.Sender
	Chat     mailer.Sender // optional
	Store    *store.Store
	FX       *fx.Rates
	// AttributionWait: the notification of a first purchase can arrive before the app's
	// purchase.started event is stored, so attribution waits up to this long.
	AttributionWait time.Duration
}

func New(cfg *config.Config, st *store.Store, sender mailer.Sender, rates *fx.Rates) *Handler {
	return &Handler{Config: cfg, Verifier: appstore.NewVerifier(), Sender: sender, Store: st, FX: rates,
		AttributionWait: 5 * time.Second}
}

// ProductName returns the configured name of a product, or its ID.
func (h *Handler) ProductName(id string) string {
	if n := h.Config.ProductName(id); n != "" {
		return n
	}
	return id
}

// ServeHTTP handles a notification. Apple retries non-200 responses after 1, 12, 24,
// 48 and 72 hours, so only a failed delivery (worth retrying) returns 5xx.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var req struct {
		SignedPayload string `json:"signedPayload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SignedPayload == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var p appstore.NotificationPayload
	if err := h.Verifier.Decode(req.SignedPayload, &p); err != nil {
		slog.Warn("reject notification: invalid signature", "error", err, "remote", r.Header.Get("X-Real-IP"))
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	app, ok := h.Config.AppByBundle(p.Data.BundleID)
	if !ok {
		slog.Warn("reject notification: unknown bundleId", "bundleId", p.Data.BundleID)
		http.Error(w, "unknown app", http.StatusBadRequest)
		return
	}

	var tx *appstore.TransactionInfo
	if p.Data.SignedTransactionInfo != "" {
		tx = &appstore.TransactionInfo{}
		if err := h.Verifier.Decode(p.Data.SignedTransactionInfo, tx); err != nil {
			slog.Warn("reject notification: invalid transaction signature", "error", err)
			http.Error(w, "invalid transaction", http.StatusBadRequest)
			return
		}
	}

	var renewal *appstore.RenewalInfo
	if p.Data.SignedRenewalInfo != "" {
		renewal = &appstore.RenewalInfo{}
		if err := h.Verifier.Decode(p.Data.SignedRenewalInfo, renewal); err != nil {
			slog.Warn("ignore renewal info: invalid signature", "error", err)
			renewal = nil
		}
	}

	in := mailInput{
		AppKey:      app.Key,
		AppName:     app.Name,
		ProductName: h.ProductName,
		ContextLabel: func(c string) string {
			if n := app.PaywallContexts[c]; n != "" {
				return n
			}
			return c
		},
		Commission: h.Config.CommissionRate,
		Currency:   h.Config.Currency,
		Payload:    &p,
		Tx:         tx,
		Renewal:    renewal,
	}
	h.record(&in)

	view := buildView(in)
	htmlBody, err := renderHTML(view)
	if err != nil {
		slog.Error("render html failed, fallback to text", "error", err)
	}
	err = h.Sender.Send(view.Subject, renderText(view), htmlBody)
	if h.Chat != nil && sendsToChat(p.NotificationType, tx) {
		err = errors.Join(err, h.Chat.Send(view.Subject, renderChat(view), ""))
	}
	if err != nil {
		slog.Error("send notification failed, Apple will retry", "error", err, "notificationUUID", p.NotificationUUID)
		http.Error(w, "notify failed", http.StatusInternalServerError)
		return
	}

	slog.Info("notification handled",
		"app", app.Key, "type", p.NotificationType, "subtype", p.Subtype,
		"environment", p.Data.Environment, "notificationUUID", p.NotificationUUID)
	w.WriteHeader(http.StatusOK)
}

// record stores the transaction and loads today's and this month's totals.
// Failures are logged only; the email is still sent.
func (h *Handler) record(in *mailInput) {
	p, tx := in.Payload, in.Tx
	ev := store.Event{
		NotificationUUID: p.NotificationUUID,
		BundleID:         p.Data.BundleID,
		Environment:      p.Data.Environment,
		NotificationType: p.NotificationType,
		Subtype:          p.Subtype,
		SignedAt:         p.SignedDate,
		App:              in.AppKey,
	}
	if tx != nil {
		ev.ProductID, ev.TransactionID, ev.Storefront = tx.ProductID, tx.TransactionID, tx.Storefront
		ev.Currency, ev.PriceMilli = tx.Currency, tx.Price
		ev.RevenueSign = RevenueSign[p.NotificationType]
		ev.AppAccountToken = tx.AppAccountToken
		ev.OriginalTransactionID, ev.TransactionReason = tx.OriginalTransactionID, tx.TransactionReason
		ev.PurchaseAt, ev.OriginalPurchaseAt = tx.PurchaseDate, tx.OriginalPurchaseDate
		ev.OfferType, ev.OfferIdentifier = tx.OfferType, tx.OfferIdentifier
		if amount, ok := h.FX.ToBase(tx.Price, tx.Currency); ok {
			in.Amount, ev.Amount = &amount, &amount
		}
	}
	if inserted, err := h.Store.Record(ev); err != nil {
		slog.Error("record event failed", "error", err, "notificationUUID", p.NotificationUUID)
	} else if !inserted {
		slog.Info("duplicate notification, not counted again", "notificationUUID", p.NotificationUUID)
	}

	conversion, err := h.Store.IsTrialConversion(p.NotificationUUID)
	if err != nil {
		slog.Error("check trial conversion failed", "error", err, "notificationUUID", p.NotificationUUID)
	}
	in.TrialConversion = conversion

	if tx != nil && tx.AppAccountToken != "" {
		in.Attribution = h.findAttribution(p.NotificationUUID, p.NotificationType)
	}

	stats, err := h.Store.Stats(p.Data.BundleID, p.Data.Environment, time.Now(), tz.Location())
	if err != nil {
		slog.Error("query stats failed", "error", err)
		return
	}
	in.Stats = &stats
}

// findAttribution looks up the purchase source of a stored notification. For a first
// purchase the app's purchase.started may still be in the write queue, so it retries
// briefly; renewals, refunds and other later notifications do not wait.
func (h *Handler) findAttribution(notificationUUID, notificationType string) *store.Attribution {
	deadline := time.Now()
	if notificationType == "ONE_TIME_CHARGE" || notificationType == "SUBSCRIBED" {
		deadline = deadline.Add(h.AttributionWait)
	}
	for {
		a, err := h.Store.FindAttribution(notificationUUID)
		if err != nil {
			slog.Error("find attribution failed", "error", err)
			return nil
		}
		if a != nil || !time.Now().Before(deadline) {
			return a
		}
		time.Sleep(500 * time.Millisecond)
	}
}
