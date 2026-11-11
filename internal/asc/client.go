// Package asc syncs sales reports from the App Store Connect API, giving the portfolio dashboard
// historical revenue and downloads for every app.
//
// It complements App Store Server Notifications (real time, but only from the day they were set up):
// reports arrive a day later but cover the full history.
// Authentication uses an API key (ES256 JWT, valid for 20 minutes), read-only.
package asc

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

const apiBase = "https://api.appstoreconnect.apple.com"

type Config struct {
	KeyID        string
	IssuerID     string
	KeyPath      string // AuthKey_XXXX.p8
	VendorNumber string
}

func (c Config) Enabled() bool {
	return c.KeyID != "" && c.IssuerID != "" && c.KeyPath != "" && c.VendorNumber != ""
}

type Client struct {
	cfg  Config
	key  *ecdsa.PrivateKey
	http *http.Client
	base string

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func NewClient(cfg Config) (*Client, error) {
	b, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("asc: read key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("asc: key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("asc: parse key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("asc: key is not ECDSA")
	}
	return &Client{cfg: cfg, key: ec, http: &http.Client{Timeout: 60 * time.Second}, base: apiBase}, nil
}

// jwt creates or reuses an ES256 token (renewed 2 minutes before expiry).
func (c *Client) jwt() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.token != "" && now.Before(c.expiry.Add(-2*time.Minute)) {
		return c.token, nil
	}
	exp := now.Add(19 * time.Minute)
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.cfg.KeyID, "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": c.cfg.IssuerID, "iat": now.Unix(), "exp": exp.Unix(), "aud": "appstoreconnect-v1"})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JWS requires r and s as fixed-length 32-byte values, concatenated
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	c.token, c.expiry = signing+"."+enc.EncodeToString(sig), exp
	return c.token, nil
}

// ErrNotFound means the report does not exist (no sales that day, or not generated yet).
var ErrNotFound = errors.New("asc: report not found")

// ErrGone means the report is past Apple's retention period (365 days for daily, about 12 months for monthly) and can no longer be fetched.
var ErrGone = errors.New("asc: report no longer available")

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("asc: HTTP %d: %s", e.Status, e.Body) }

func (c *Client) get(path string, q url.Values) ([]byte, error) {
	return c.getAccept(path, q, "application/json")
}

func (c *Client) getAccept(path string, q url.Values, accept string) ([]byte, error) {
	tok, err := c.jwt()
	if err != nil {
		return nil, err
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", accept)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusNotFound:
			return nil, ErrNotFound
		case resp.StatusCode == http.StatusGone:
			return nil, ErrGone
		case resp.StatusCode == http.StatusTooManyRequests && attempt < 3:
			time.Sleep(time.Duration(attempt+1) * 30 * time.Second)
			continue
		}
		msg := string(body)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, &apiError{Status: resp.StatusCode, Body: msg}
	}
}

// App is an app in the account.
type App struct {
	ID       string // Apple ID (numeric)
	BundleID string
	SKU      string
	Name     string
	Notified bool // has a production App Store server notification URL, so its sales arrive live
}

// Apps lists every app in the account, used to map report rows to a bundle ID.
func (c *Client) Apps() ([]App, error) {
	var out []App
	q := url.Values{"fields[apps]": {"bundleId,sku,name,subscriptionStatusUrl"}, "limit": {"200"}}
	body, err := c.get("/v1/apps", q)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				BundleID string `json:"bundleId"`
				SKU      string `json:"sku"`
				Name     string `json:"name"`
				URL      string `json:"subscriptionStatusUrl"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	for _, d := range resp.Data {
		out = append(out, App{ID: d.ID, BundleID: d.Attributes.BundleID, SKU: d.Attributes.SKU, Name: d.Attributes.Name,
			Notified: d.Attributes.URL != ""})
	}
	return out, nil
}
