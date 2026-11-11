package asc

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RedHiwiK/HiwiKInsight/internal/store"
)

const sampleReport = "Provider\tProvider Country\tSKU\tDeveloper\tTitle\tVersion\tProduct Type Identifier\tUnits\tDeveloper Proceeds\tBegin Date\tEnd Date\tCustomer Currency\tCountry Code\tCurrency of Proceeds\tApple Identifier\tCustomer Price\tPromo Code\tParent Identifier\tSubscription\tPeriod\n" +
	"APPLE\tUS\tcom.example.pawprint\tExample\tPawprint\t2.4.4\t1F\t12\t0\t09/20/2026\t09/20/2026\tCNY\tCN\tCNY\t1234567890\t0\t\t\t\t\n" +
	"APPLE\tUS\tpro.lifetime\tExample\tPro\t\tIA1\t2\t15.30\t09/20/2026\t09/20/2026\tCNY\tCN\tCNY\t111\t18\t\tcom.example.pawprint\t\t\n" +
	"APPLE\tUS\tpro.monthly\tExample\tPro\t\tIAY\t-1\t1.49\t09/20/2026\t09/20/2026\tUSD\tUS\tUSD\t222\t1.99\t\tcom.example.trailmark\tRenewal\t1 Month\n"

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestParseSales(t *testing.T) {
	rows, err := ParseSales(gz(sampleReport))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].Units != 12 || Category(rows[0].ProductType) != "download" {
		t.Errorf("download row = %+v", rows[0])
	}
	if rows[1].ParentSKU != "com.example.pawprint" || rows[1].ProceedsPerUnit != 15.3 || Category(rows[1].ProductType) != "iap" {
		t.Errorf("iap row = %+v", rows[1])
	}
	if rows[2].Units != -1 || rows[2].Subscription != "Renewal" || Category(rows[2].ProductType) != "subscription" {
		t.Errorf("refund row = %+v", rows[2])
	}
}

func TestCategory(t *testing.T) {
	for pt, want := range map[string]string{"1": "download", "1F": "download", "3": "redownload", "7F": "update",
		"IAY": "subscription", "IAY-M": "subscription", "IA1": "iap", "IA9-M": "iap", "XYZ": "other"} {
		if got := Category(pt); got != want {
			t.Errorf("Category(%s) = %s, want %s", pt, got, want)
		}
	}
}

// Uses a test server to check that the JWT verifies with the public key, sales reports send a gzip Accept header, 404 maps to ErrNotFound and 410 maps to ErrGone
func TestClientRequests(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "AuthKey.p8")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(tok, ".")
		if len(parts) != 3 {
			http.Error(w, "bad token", 401)
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if len(sig) != 64 || !ecdsa.Verify(&key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			http.Error(w, "bad signature", 401)
			return
		}
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var c map[string]any
		json.Unmarshal(claims, &c)
		if c["aud"] != "appstoreconnect-v1" || c["iss"] != "issuer" {
			http.Error(w, "bad claims", 401)
			return
		}
		switch {
		case r.URL.Path == "/v1/salesReports" && r.URL.Query().Get("filter[reportDate]") == "2026-09-20":
			if r.Header.Get("Accept") != "application/a-gzip" || r.URL.Query().Get("filter[vendorNumber]") != "123" {
				http.Error(w, "bad request", 406)
				return
			}
			w.Write(gz(sampleReport))
		case r.URL.Path == "/v1/salesReports" && r.URL.Query().Get("filter[reportDate]") == "2024-01":
			http.Error(w, `{"errors":[{"status":"410","code":"GONE_ERROR"}]}`, http.StatusGone)
		case r.URL.Path == "/v1/salesReports":
			http.NotFound(w, r)
		default:
			http.Error(w, "unexpected", 500)
		}
	}))
	defer srv.Close()

	c, err := NewClient(Config{KeyID: "KID", IssuerID: "issuer", KeyPath: path, VendorNumber: "123"})
	if err != nil {
		t.Fatal(err)
	}
	c.base = srv.URL
	rows, err := c.Sales("DAILY", "2026-09-20")
	if err != nil || len(rows) != 3 {
		t.Fatalf("Sales = %d rows, %v", len(rows), err)
	}
	if _, err := c.Sales("DAILY", "2026-09-21"); err != ErrNotFound {
		t.Errorf("missing day err = %v, want ErrNotFound", err)
	}
	if _, err := c.Sales("MONTHLY", "2024-01"); err != ErrGone {
		t.Errorf("expired month err = %v, want ErrGone", err)
	}
}

func TestAggregateFunnel(t *testing.T) {
	disc := "Date\tApp Name\tApp Apple Identifier\tEvent\tPage Type\tSource Type\tEngagement Type\tDevice\tPlatform Version\tTerritory\tCounts\tUnique Counts\n" +
		"2026-09-22\tA\t1\tImpression\tProduct page\tApp Store search\t\tiPhone\tiOS 26\tCN\t10\t8\n" +
		"2026-09-22\tA\t1\tImpression\tProduct page\tApp Store search\t\tiPad\tiOS 26\tCN\t5\t4\n" +
		"2026-09-22\tA\t1\tPage view\tProduct page\tApp Store search\t\tiPhone\tiOS 26\tCN\t3\t2\n" +
		"2026-09-22\tA\t1\tTap\tProduct page\tApp Store browse\t\tiPhone\tiOS 26\tCN\t7\t7\n" +
		"2026-09-22\tA\t1\tImpression\tProduct page\tInstitutional purchase\t\tiPhone\tiOS 26\tUS\t1\t1\n"
	rows, err := ParseTSV(gz(disc))
	if err != nil {
		t.Fatal(err)
	}
	agg := AggregateFunnel(ReportDiscovery, rows)
	cn := agg[FunnelKey{"2026-09-22", "1", "search", "CN"}]
	if cn == nil || cn.Impressions != 15 || cn.ImpressionsUnique != 12 || cn.PageViews != 3 || cn.PageViewsUnique != 2 {
		t.Errorf("search/CN = %+v", cn)
	}
	if us := agg[FunnelKey{"2026-09-22", "1", "other", "US"}]; us == nil || us.Impressions != 1 {
		t.Errorf("other/US = %+v", us)
	}
	// Taps are not counted in any column
	if b := agg[FunnelKey{"2026-09-22", "1", "browse", "CN"}]; b != nil && (b.Impressions != 0 || b.PageViews != 0) {
		t.Errorf("tap counted: %+v", b)
	}

	dl := "Date\tApp Name\tApp Apple Identifier\tDownload Type\tApp Version\tDevice\tPlatform Version\tSource Type\tPage Type\tPre-Order\tTerritory\tCounts\n" +
		"2026-09-22\tA\t1\tFirst-time download\t2.4.4\tiPhone\tiOS 26\tApp Store search\t\t\tCN\t6\n" +
		"2026-09-22\tA\t1\tRedownload\t2.4.4\tiPhone\tiOS 26\tApp Store search\t\t\tCN\t2\n" +
		"2026-09-22\tA\t1\tAuto-update\t2.4.4\tiPhone\tiOS 26\tApp Store search\t\t\tCN\t44\n"
	rows, err = ParseTSV([]byte(dl))
	if err != nil {
		t.Fatal(err)
	}
	d := AggregateFunnel(ReportDownloads, rows)[FunnelKey{"2026-09-22", "1", "search", "CN"}]
	if d == nil || d.FirstDownloads != 6 || d.Redownloads != 2 {
		t.Errorf("downloads = %+v", d)
	}
}

func TestSalesMessage(t *testing.T) {
	p := func(v float64) *float64 { return &v }
	recs := []store.SalesRecord{
		{BundleID: "com.example.quiet", Title: "Unlock", Category: "iap", Units: 1, ProceedsPerUnit: 8.49, Proceeds: p(60.5), CustomerPrice: 9.99, CustomerCurrency: "EUR", Country: "NL"},
		{BundleID: "com.example.quiet", Title: "Unlock", Category: "iap", Units: -1, ProceedsPerUnit: 8.49, Proceeds: p(-60.5), CustomerPrice: 9.99, CustomerCurrency: "EUR", Country: "DE"},
		{BundleID: "com.example.quiet", Title: "Quiet", Category: "download", Units: 3},
		{BundleID: "com.example.quiet", Title: "Pro", Category: "subscription", Units: 1, ProceedsPerUnit: 0}, // free trial
		{BundleID: "com.example.live", Title: "Pro", Category: "subscription", Units: 1, ProceedsPerUnit: 1.49, Proceeds: p(10.6)},
	}
	names := map[string]string{"com.example.quiet": "Quiet", "com.example.live": "Live"}
	live := map[string]bool{"com.example.live": true}
	subject, body, ok := salesMessage("2026-11-09", recs, names, live, "CNY")
	if !ok || !strings.Contains(subject, "2026-11-09") {
		t.Fatalf("subject %q ok %v", subject, ok)
	}
	lines := strings.Split(body, "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "NL") || !strings.Contains(lines[1], "DE") || strings.Contains(body, "Live") {
		t.Fatalf("body %q", body)
	}
	if _, _, ok := salesMessage("2026-11-09", recs[2:], names, live, "CNY"); ok {
		t.Fatal("downloads, free trials and apps with live notifications are not news")
	}
}
