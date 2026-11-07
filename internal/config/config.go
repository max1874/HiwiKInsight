// Package config loads the single configuration file of a HiwiKInsight deployment.
//
// Everything lives in one YAML file (see examples/config.example.yaml). Any value may
// reference an environment variable as ${NAME} or ${NAME:-default}, which is the
// recommended way to keep secrets (SMTP password, query tokens, App Store Connect
// keys) out of the file.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Listen is the HTTP address, e.g. ":8080" or "127.0.0.1:8080".
	Listen string `yaml:"listen"`
	// PublicURL is how users and the SDK reach this server. Informational: printed by
	// check-config and at startup, so you can verify the URL to give to the SDK and App Store Connect.
	PublicURL string `yaml:"public_url"`
	// DataDir holds the SQLite database and the session secret. Defaults to
	// $HIWIKINSIGHT_DATA_DIR (/data in the container image), then ./data.
	// Relative paths in this file (data_dir, catalogs, key_path) are resolved against
	// the directory of the config file, not the working directory.
	DataDir string `yaml:"data_dir"`
	// Timezone is the IANA zone that defines a "day" for every metric, report and alert.
	Timezone string `yaml:"timezone"`
	// Language of emails, reports and alerts: "en" or "zh".
	Language string `yaml:"language"`
	// Currency is the base currency revenue is converted to and stored in (ISO 4217).
	Currency string `yaml:"currency"`
	// CommissionRate is Apple's cut used to estimate proceeds in transaction emails
	// (0.15 for the Small Business Program, 0.30 otherwise).
	CommissionRate float64 `yaml:"commission_rate"`
	// RetentionDays is how long raw events are kept. Derived tables (installs, daily
	// activity, purchase attribution) are kept forever.
	RetentionDays int `yaml:"retention_days"`

	Apps      []App     `yaml:"apps"`
	Dashboard Dashboard `yaml:"dashboard"`
	Query     Query     `yaml:"query"`
	Ingest    Ingest    `yaml:"ingest"`
	Mail      Mail      `yaml:"mail"`
	Feishu    Feishu    `yaml:"feishu"`
	Reports   Reports   `yaml:"reports"`
	Alerts    Alerts    `yaml:"alerts"`
	ASC       ASC       `yaml:"app_store_connect"`

	// Dir is the directory of the config file (set by Load).
	Dir string `yaml:"-"`
}

// App is one app sending events and/or App Store data to this server.
type App struct {
	// Key identifies the app everywhere (SDK appKey, query parameter ?app=, CLI --app).
	Key string `yaml:"key"`
	// BundleID links App Store notifications and App Store Connect reports to the app.
	BundleID string `yaml:"bundle_id"`
	// Name is the display name; defaults to Key.
	Name string `yaml:"name"`
	// Color is a dashboard palette slot: purple, teal, orange, pink, blue, violet, green, gold.
	// Apps without a color take the remaining slots in config order.
	Color string `yaml:"color"`
	// Catalog is the path to the app's event catalog (YAML), relative to the config file.
	Catalog string `yaml:"catalog"`
	// Products maps product IDs to display names used in emails and the dashboard.
	Products map[string]string `yaml:"products"`
	// PaywallContexts maps the SDK's purchase context values to display names.
	PaywallContexts map[string]string `yaml:"paywall_contexts"`
}

type Dashboard struct {
	// Enabled serves the web dashboard under /dashboard.
	Enabled *bool `yaml:"enabled"`
	// Users who can sign in. Create a hash with `hiwikinsight hash-password`.
	Users []User `yaml:"users"`
	// SessionTTL is how long a sign-in lasts, e.g. "720h".
	SessionTTL Duration `yaml:"session_ttl"`
	// InsecureNoAuth disables sign-in. Only allowed when Listen is a loopback address.
	InsecureNoAuth bool `yaml:"insecure_no_auth"`
}

type User struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

type Query struct {
	// Tokens accepted as "Authorization: Bearer <token>" on /v1/query/*.
	// Without tokens the API is only reachable from a signed-in dashboard session.
	Tokens []string `yaml:"tokens"`
}

type Ingest struct {
	// DailyCap is the maximum number of events accepted per install per day (abuse guard).
	DailyCap int `yaml:"daily_cap"`
}

type Mail struct {
	SMTPHost string   `yaml:"smtp_host"`
	SMTPPort int      `yaml:"smtp_port"`
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
}

// Enabled reports whether mail can be sent; otherwise mail is only logged.
func (m Mail) Enabled() bool { return m.SMTPHost != "" && len(m.To) > 0 }

// Feishu holds the app bot that messages one person about charges and refunds.
type Feishu struct {
	// BaseURL is https://open.feishu.cn (default) or https://open.larksuite.com for Lark.
	BaseURL   string `yaml:"base_url"`
	AppID     string `yaml:"app_id"`
	AppSecret string `yaml:"app_secret"`
	// OpenID is the recipient's open_id under this app.
	OpenID string `yaml:"open_id"`
}

// Enabled reports whether all credentials are present.
func (f Feishu) Enabled() bool { return f.AppID != "" && f.AppSecret != "" && f.OpenID != "" }

type Reports struct {
	Daily  *bool `yaml:"daily"`
	Weekly *bool `yaml:"weekly"`
	// Hour (0–23, reporting time zone) after which the daily report for yesterday is sent.
	Hour *int `yaml:"hour"`
}

type Alerts struct {
	Enabled *bool `yaml:"enabled"`
	// SpikeUsers: an existing error affecting at least this many users within an hour alerts.
	SpikeUsers int `yaml:"spike_users"`
	// ReportStaleDays: alert when the newest App Store Connect daily report is older than this.
	ReportStaleDays int `yaml:"report_stale_days"`
	// SilentHours: alert when an app that had events in the previous week sends nothing for this long.
	SilentHours int `yaml:"silent_hours"`
	// PaymentErrors are error ids that alert on the first occurrence.
	PaymentErrors []string `yaml:"payment_errors"`
}

// ASC holds App Store Connect API credentials for sales and analytics reports.
type ASC struct {
	KeyID        string `yaml:"key_id"`
	IssuerID     string `yaml:"issuer_id"`
	KeyPath      string `yaml:"key_path"`
	VendorNumber string `yaml:"vendor_number"`
}

// Enabled reports whether all credentials are present.
func (a ASC) Enabled() bool {
	return a.KeyID != "" && a.IssuerID != "" && a.KeyPath != "" && a.VendorNumber != ""
}

// Duration accepts Go duration strings such as "720h" or "30m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	d.Duration = v
	return nil
}

var envRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${NAME} and ${NAME:-default} in one scalar value.
func expandEnv(s string) string {
	return envRE.ReplaceAllStringFunc(s, func(m string) string {
		p := envRE.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(p[1]); ok && v != "" {
			return v
		}
		return p[3]
	})
}

// Load reads, expands, defaults and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.Dir = filepath.Dir(abs)
	c.DataDir = c.Resolve(c.DataDir)
	c.ASC.KeyPath = c.Resolve(c.ASC.KeyPath)
	for i := range c.Apps {
		c.Apps[i].Catalog = c.Resolve(c.Apps[i].Catalog)
	}
	return c, nil
}

// Parse decodes YAML, expands environment variables, applies defaults and validates.
func Parse(b []byte) (*Config, error) {
	var c Config
	var doc yaml.Node
	if err := yaml.NewDecoder(strings.NewReader(string(b))).Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if doc.Kind != 0 { // an empty file means all defaults
		// Expanding the parsed document rather than the raw text keeps a value containing
		// "#", ": " or quotes a single literal string instead of changing the YAML structure.
		expandNode(&doc)
		// yaml.Node.Decode has no KnownFields option, so unknown keys are checked separately.
		if err := checkKnownFields(&doc, reflect.TypeOf(c)); err != nil {
			return nil, err
		}
		if err := doc.Decode(&c); err != nil {
			return nil, err
		}
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func expandNode(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && envRE.MatchString(n.Value) {
		n.Value = expandEnv(n.Value)
		quoted := yaml.SingleQuotedStyle | yaml.DoubleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle | yaml.TaggedStyle
		if n.Style&quoted == 0 {
			n.Tag = "" // re-resolve, so a plain ${PORT:-587} still decodes into an int
		}
	}
	for _, child := range n.Content {
		expandNode(child)
	}
}

var unmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()

// checkKnownFields rejects mapping keys that match no yaml field of the target struct.
func checkKnownFields(n *yaml.Node, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	switch {
	case n.Kind == yaml.DocumentNode:
		for _, child := range n.Content {
			if err := checkKnownFields(child, t); err != nil {
				return err
			}
		}
	case n.Kind == yaml.SequenceNode && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array):
		for _, child := range n.Content {
			if err := checkKnownFields(child, t.Elem()); err != nil {
				return err
			}
		}
	case n.Kind == yaml.MappingNode && t.Kind() == reflect.Map:
		for i := 1; i < len(n.Content); i += 2 {
			if err := checkKnownFields(n.Content[i], t.Elem()); err != nil {
				return err
			}
		}
	case n.Kind == yaml.MappingNode && t.Kind() == reflect.Struct:
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if !f.IsExported() || name == "-" {
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			fields[name] = f.Type
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			ft, ok := fields[key.Value]
			if !ok {
				return fmt.Errorf("line %d: field %s not found in type config.%s", key.Line, key.Value, t.Name())
			}
			if err := checkKnownFields(n.Content[i+1], ft); err != nil {
				return err
			}
		}
	}
	return nil
}

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = os.Getenv("HIWIKINSIGHT_DATA_DIR") // set to /data in the container image
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if c.Language == "" {
		c.Language = "en"
	}
	c.Currency = strings.ToUpper(c.Currency)
	if c.Currency == "" {
		c.Currency = "USD"
	}
	if c.CommissionRate == 0 {
		c.CommissionRate = 0.15
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 365
	}
	if c.Dashboard.Enabled == nil {
		c.Dashboard.Enabled = boolPtr(true)
	}
	if c.Dashboard.SessionTTL.Duration == 0 {
		c.Dashboard.SessionTTL.Duration = 30 * 24 * time.Hour
	}
	if c.Ingest.DailyCap == 0 {
		c.Ingest.DailyCap = 5000
	}
	if c.Mail.SMTPPort == 0 {
		c.Mail.SMTPPort = 465
	}
	if c.Mail.From == "" {
		c.Mail.From = c.Mail.Username
	}
	if c.Feishu.BaseURL == "" {
		c.Feishu.BaseURL = "https://open.feishu.cn"
	}
	if c.Reports.Daily == nil {
		c.Reports.Daily = boolPtr(true)
	}
	if c.Reports.Weekly == nil {
		c.Reports.Weekly = boolPtr(true)
	}
	if c.Reports.Hour == nil {
		c.Reports.Hour = intPtr(9)
	}
	if c.Alerts.Enabled == nil {
		c.Alerts.Enabled = boolPtr(true)
	}
	if c.Alerts.SpikeUsers == 0 {
		c.Alerts.SpikeUsers = 3
	}
	if c.Alerts.ReportStaleDays == 0 {
		c.Alerts.ReportStaleDays = 3
	}
	if c.Alerts.SilentHours == 0 {
		c.Alerts.SilentHours = 48
	}
	if c.Alerts.PaymentErrors == nil {
		c.Alerts.PaymentErrors = []string{"purchase", "restore"}
	}
	for i := range c.Apps {
		if c.Apps[i].Name == "" {
			c.Apps[i].Name = c.Apps[i].Key
		}
	}
}

var (
	keyRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	colorOK = map[string]bool{"": true, "purple": true, "teal": true, "orange": true, "pink": true,
		"blue": true, "violet": true, "green": true, "gold": true}
)

func (c *Config) validate() error {
	var errs []string
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Sprintf("timezone: %v", err))
	}
	if c.Language != "en" && c.Language != "zh" {
		errs = append(errs, `language must be "en" or "zh"`)
	}
	if len(c.Currency) != 3 {
		errs = append(errs, "currency must be an ISO 4217 code such as USD")
	}
	if c.RetentionDays < 1 {
		errs = append(errs, "retention_days must be at least 1")
	}
	if c.Ingest.DailyCap < 1 {
		errs = append(errs, "ingest.daily_cap must be at least 1")
	}
	if c.CommissionRate < 0 || c.CommissionRate >= 1 {
		errs = append(errs, "commission_rate must be in [0, 1)")
	}
	if *c.Reports.Hour < 0 || *c.Reports.Hour > 23 {
		errs = append(errs, "reports.hour must be 0–23")
	}
	if f := c.Feishu; !f.Enabled() && f.AppID+f.AppSecret+f.OpenID != "" {
		errs = append(errs, "feishu: app_id, app_secret and open_id must be set together")
	}
	keys, bundles := map[string]bool{}, map[string]bool{}
	for i, a := range c.Apps {
		switch {
		case !keyRE.MatchString(a.Key):
			errs = append(errs, fmt.Sprintf("apps[%d].key %q: use lowercase letters, digits, - and _", i, a.Key))
		case keys[a.Key]:
			errs = append(errs, fmt.Sprintf("apps[%d].key %q is duplicated", i, a.Key))
		}
		keys[a.Key] = true
		if a.BundleID == "" {
			errs = append(errs, fmt.Sprintf("apps[%d] (%s): bundle_id is required", i, a.Key))
		} else if bundles[a.BundleID] {
			errs = append(errs, fmt.Sprintf("apps[%d] (%s): bundle_id %q is duplicated", i, a.Key, a.BundleID))
		}
		bundles[a.BundleID] = true
		if !colorOK[a.Color] {
			errs = append(errs, fmt.Sprintf("apps[%d] (%s): unknown color %q", i, a.Key, a.Color))
		}
	}
	for i, u := range c.Dashboard.Users {
		if u.Username == "" || !strings.HasPrefix(u.PasswordHash, "$2") {
			errs = append(errs, fmt.Sprintf("dashboard.users[%d]: username and a bcrypt password_hash are required (hiwikinsight hash-password)", i))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// AppByKey returns the app with the given key.
func (c *Config) AppByKey(key string) (App, bool) {
	for _, a := range c.Apps {
		if a.Key == key {
			return a, true
		}
	}
	return App{}, false
}

// AppByBundle returns the app with the given bundle ID.
func (c *Config) AppByBundle(bundle string) (App, bool) {
	for _, a := range c.Apps {
		if a.BundleID == bundle {
			return a, true
		}
	}
	return App{}, false
}

// ProductName returns the configured display name of a product, or "" when none.
func (c *Config) ProductName(productID string) string {
	for _, a := range c.Apps {
		if n := a.Products[productID]; n != "" {
			return n
		}
	}
	return ""
}

// DBPath is the SQLite database file inside DataDir.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "hiwikinsight.db") }

// Resolve makes a path from the config file absolute (relative to the config's directory).
func (c *Config) Resolve(p string) string {
	if p == "" || filepath.IsAbs(p) || c.Dir == "" {
		return p
	}
	return filepath.Join(c.Dir, p)
}
