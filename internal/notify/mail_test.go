package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/appstore"
	"github.com/RedHiwiK/HiwiKInsight/internal/i18n"
	"github.com/RedHiwiK/HiwiKInsight/internal/store"
	"github.com/RedHiwiK/HiwiKInsight/internal/tz"
)

func TestMain(m *testing.M) {
	tz.MustSet("America/New_York")
	os.Exit(m.Run())
}

var testProducts = map[string]string{
	"com.example.pawprint.pro.lifetime": "Lifetime",
	"com.example.ledger.pro.yearly":     "Yearly",
	"com.example.ledger.pro.monthly":    "Monthly",
}

func productName(id string) string {
	if n := testProducts[id]; n != "" {
		return n
	}
	return id
}

var contexts = map[string]string{"record_limit": "Record limit"}

func contextLabel(c string) string {
	if n := contexts[c]; n != "" {
		return n
	}
	return c
}

func payload(typ, subtype, bundle, env string) *appstore.NotificationPayload {
	p := &appstore.NotificationPayload{NotificationType: typ, Subtype: subtype, NotificationUUID: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", SignedDate: time.Now().UnixMilli()}
	p.Data.BundleID, p.Data.Environment = bundle, env
	return p
}

func f(v float64) *float64 { return &v }

func scenarios() map[string]mailInput {
	now := time.Now().UnixMilli()
	reason := 1
	base := func(in mailInput) mailInput {
		in.ProductName, in.ContextLabel, in.Commission, in.Currency = productName, contextLabel, 0.15, "USD"
		return in
	}
	return map[string]mailInput{
		"purchase": base(mailInput{
			AppName: "Pawprint",
			Payload: payload("ONE_TIME_CHARGE", "", "com.example.pawprint", "Production"),
			Tx: &appstore.TransactionInfo{TransactionID: "2000000812345678", OriginalTransactionID: "2000000812345678",
				ProductID: "com.example.pawprint.pro.lifetime", Type: "Non-Consumable", Storefront: "USA",
				Currency: "USD", Price: 9990, PurchaseDate: now, TransactionReason: "PURCHASE", InAppOwnershipType: "PURCHASED",
				AppAccountToken: "6f1c2b8e-0d4a-4f7e-9a51-3c2b1d0e9f8a"},
			Amount: f(9.99),
			Stats:  &store.Stats{Today: store.Period{Sales: 3, Net: 29.97}, Month: store.Period{Sales: 41, Refunds: 1, Net: 399.6}},
			Attribution: &store.Attribution{Context: "record_limit", InstallDay: 12, PaywallViews: 3, AppVersion: "2.4.5",
				Device: "iPhone 16 Pro", OSVersion: "26.1", InstallID: "5b0c2f4e-1111-4a2b-9c3d-000000000001"},
		}),
		"renewal-eur": base(mailInput{
			AppName: "Ledger",
			Payload: payload("DID_RENEW", "", "com.example.ledger", "Sandbox"),
			Tx: &appstore.TransactionInfo{TransactionID: "2000000899990002", OriginalTransactionID: "2000000899990001",
				ProductID: "com.example.ledger.pro.yearly", Type: "Auto-Renewable Subscription", Storefront: "DEU",
				Currency: "EUR", Price: 19990, PurchaseDate: now, OriginalPurchaseDate: now - 365*24*3600*1000,
				ExpiresDate: now + 365*24*3600*1000, TransactionReason: "RENEWAL", OfferType: 1, OfferDiscountType: "PAY_UP_FRONT"},
			Renewal: &appstore.RenewalInfo{AutoRenewStatus: 1, AutoRenewProductID: "com.example.ledger.pro.monthly", RenewalPrice: 2990, Currency: "EUR"},
			Amount:  f(21.7),
			Stats:   &store.Stats{Today: store.Period{Sales: 1, Net: 21.7}, Month: store.Period{Sales: 6, Net: 110.5, Unconverted: 1}},
		}),
		"refund": base(mailInput{
			AppName: "Pawprint",
			Payload: payload("REFUND", "", "com.example.pawprint", "Production"),
			Tx: &appstore.TransactionInfo{TransactionID: "2000000812345678", OriginalTransactionID: "2000000812345678",
				ProductID: "com.example.pawprint.pro.lifetime", Type: "Non-Consumable", Storefront: "HKG",
				Currency: "HKD", Price: 78000, PurchaseDate: now - 3*24*3600*1000, RevocationDate: now, RevocationReason: &reason},
			Amount: f(10.0),
			Stats:  &store.Stats{Today: store.Period{Sales: 2, Refunds: 1, Net: 9.98}, Month: store.Period{Sales: 41, Refunds: 2, Net: 389.6}},
		}),
		// Only a subscription starts a free trial; a lifetime product redeemed at price 0 is just free
		"trial-start": base(mailInput{
			AppName: "Ledger",
			Payload: payload("SUBSCRIBED", "INITIAL_BUY", "com.example.ledger", "Production"),
			Tx: &appstore.TransactionInfo{TransactionID: "2000000899990003", OriginalTransactionID: "2000000899990003",
				ProductID: "com.example.ledger.pro.yearly", Type: "Auto-Renewable Subscription", Storefront: "USA",
				Currency: "USD", Price: 0, PurchaseDate: now, ExpiresDate: now + 7*24*3600*1000, TransactionReason: "PURCHASE", OfferType: 1},
			Amount: f(0),
			Stats:  &store.Stats{Today: store.Period{Trials: 1}},
		}),
		"free-lifetime": base(mailInput{
			AppName: "Pawprint",
			Payload: payload("ONE_TIME_CHARGE", "", "com.example.pawprint", "Production"),
			Tx: &appstore.TransactionInfo{TransactionID: "2000000812349999", OriginalTransactionID: "2000000812349999",
				ProductID: "com.example.pawprint.pro.lifetime", Type: "Non-Consumable", Storefront: "CHN",
				Currency: "CNY", Price: 0, PurchaseDate: now, TransactionReason: "PURCHASE", OfferType: 3, OfferIdentifier: "Lifetime giveaway"},
			Amount: f(0),
			Stats:  &store.Stats{},
		}),
		"test": base(mailInput{
			AppName: "Pawprint",
			Payload: payload("TEST", "", "com.example.pawprint", "Sandbox"),
			Stats:   &store.Stats{},
		}),
	}
}

// Covers purchase / foreign-currency renewal / refund / test emails in both languages.
// With PREVIEW_DIR set, the HTML is also written there for a visual check.
func TestRenderScenarios(t *testing.T) {
	defer i18n.Set("en")
	dir := os.Getenv("PREVIEW_DIR")
	for _, lang := range []string{"en", "zh"} {
		i18n.Set(lang)
		for name, in := range scenarios() {
			v := buildView(in)
			html, err := renderHTML(v)
			if err != nil {
				t.Fatalf("%s/%s: render html: %v", lang, name, err)
			}
			text := renderText(v)
			if v.Subject == "" || !strings.Contains(html, in.AppName) || !strings.Contains(text, in.AppName) {
				t.Fatalf("%s/%s: incomplete output", lang, name)
			}
			if dir != "" {
				os.WriteFile(filepath.Join(dir, lang+"-"+name+".html"), []byte("<!-- "+v.Subject+" -->\n"+html), 0o644)
				os.WriteFile(filepath.Join(dir, lang+"-"+name+".txt"), []byte(v.Subject+"\n\n"+text), 0o644)
			}
		}
	}

	i18n.Set("en")
	s := scenarios()
	if v := buildView(s["purchase"]); v.Subject != "Pawprint · Purchase +$9.99 Lifetime (United States) · sale #3 today" {
		t.Errorf("unexpected purchase subject: %q", v.Subject)
	}
	if v := buildView(s["refund"]); v.Amount != "-HK$78.00" || !strings.Contains(v.AmountNote, "≈ $10.00") {
		t.Errorf("unexpected refund amount: %q / %q", v.Amount, v.AmountNote)
	}
	if v := buildView(s["trial-start"]); v.Amount != "Free trial" {
		t.Errorf("trial start amount = %q", v.Amount)
	}
	if v := buildView(s["free-lifetime"]); v.Amount != "Code redeemed" {
		t.Errorf("offer-code lifetime amount = %q", v.Amount)
	}
	free := s["free-lifetime"]
	tx := *free.Tx
	tx.OfferType, free.Tx = 0, &tx
	if v := buildView(free); v.Amount != "Free" {
		t.Errorf("free lifetime amount = %q", v.Amount)
	}
	text := renderText(buildView(s["purchase"]))
	for _, want := range []string{"Entry point: Record limit", "Install age: day 12", "Paywall views: paywall view #3", "Device: iPhone 16 Pro / iOS 26.1"} {
		if !strings.Contains(text, want) {
			t.Errorf("purchase mail missing %q:\n%s", want, text)
		}
	}
	if text := renderText(buildView(s["refund"])); !strings.Contains(text, "Entry point: Unattributed") {
		t.Errorf("refund mail should be unattributed:\n%s", text)
	}

	i18n.Set("zh")
	if v := buildView(s["purchase"]); v.Subject != "Pawprint · 新购买 +$9.99 Lifetime (美国) · 今日第 3 笔" {
		t.Errorf("unexpected zh purchase subject: %q", v.Subject)
	}
}

func TestChatCoversChargesRefundsAndTest(t *testing.T) {
	for typ, want := range map[string]bool{
		"ONE_TIME_CHARGE": true, "SUBSCRIBED": true, "DID_RENEW": true, "REFUND": true, "REFUND_REVERSED": true, "TEST": true,
		"EXPIRED": false, "DID_CHANGE_RENEWAL_STATUS": false, "DID_FAIL_TO_RENEW": false,
	} {
		if got := sendsToChat(typ, nil); got != want {
			t.Errorf("sendsToChat(%s) = %v", typ, got)
		}
	}
	if sendsToChat("SUBSCRIBED", &appstore.TransactionInfo{OfferType: 1}) {
		t.Error("a free-trial start was sent to chat")
	}
	if !sendsToChat("SUBSCRIBED", &appstore.TransactionInfo{Price: 28000}) {
		t.Error("a paid subscription was not sent to chat")
	}
}

func TestRenderChat(t *testing.T) {
	defer i18n.Set("en")
	i18n.Set("en")
	v := buildView(scenarios()["purchase"])
	got := renderChat(v)
	for _, want := range []string{v.AmountNote, "Today: ", "This month: "} {
		if !strings.Contains(got, want) {
			t.Errorf("chat body misses %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Notification ID") {
		t.Errorf("chat body carries notification metadata:\n%s", got)
	}
	if got := renderChat(buildView(mailInput{AppName: "Pawprint", Payload: payload("TEST", "", "com.example.pawprint", "Sandbox")})); got != "" {
		t.Errorf("test notification should only have a subject, got:\n%s", got)
	}
}

// Apple sends the first charge after a free trial as DID_RENEW; only that charge is a conversion.
func TestTrialConversion(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	day := int64(24 * time.Hour / time.Millisecond)
	record := func(uuid, original, typ string, price, at int64) {
		t.Helper()
		if _, err := st.Record(store.Event{NotificationUUID: uuid, BundleID: "com.example.ledger", Environment: "Production",
			NotificationType: typ, RevenueSign: RevenueSign[typ], PriceMilli: price, SignedAt: at, App: "ledger",
			OriginalTransactionID: original}); err != nil {
			t.Fatal(err)
		}
	}
	record("trial", "with-trial", "SUBSCRIBED", 0, 0)
	record("first", "with-trial", "DID_RENEW", 28000, 7*day)
	record("second", "with-trial", "DID_RENEW", 28000, 372*day)
	record("paid-start", "no-trial", "SUBSCRIBED", 28000, 0)
	record("renewal", "no-trial", "DID_RENEW", 28000, 365*day)

	for uuid, want := range map[string]bool{"trial": false, "first": true, "second": false, "paid-start": false, "renewal": false, "missing": false} {
		got, err := st.IsTrialConversion(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("IsTrialConversion(%s) = %v", uuid, got)
		}
	}

	defer i18n.Set("en")
	i18n.Set("en")
	in := scenarios()["renewal-eur"]
	in.TrialConversion = true
	if v := buildView(in); v.Label != "Trial converted" || !strings.Contains(v.Subject, "Trial converted") {
		t.Errorf("label %q, subject %q", v.Label, v.Subject)
	}
}
