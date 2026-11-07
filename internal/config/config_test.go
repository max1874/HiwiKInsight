package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAndEnvExpansion(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("TEST_SMTP_PASSWORD", "s3cret")
	defer os.Unsetenv("TEST_SMTP_PASSWORD")
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte(`
timezone: Asia/Tokyo
mail:
  smtp_host: smtp.example.com
  password: ${TEST_SMTP_PASSWORD}
  username: ${TEST_UNSET:-bot@example.com}
  to: [me@example.com]
apps:
  - key: pawprint
    bundle_id: com.example.pawprint
    catalog: catalogs/pawprint.yaml
`), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mail.Password != "s3cret" || c.Mail.Username != "bot@example.com" || c.Mail.From != "bot@example.com" {
		t.Errorf("env expansion: %+v", c.Mail)
	}
	if c.Currency != "USD" || c.Language != "en" || c.Listen != ":8080" || *c.Reports.Hour != 9 || c.Alerts.SpikeUsers != 3 {
		t.Errorf("defaults: %+v", c)
	}
	if c.Apps[0].Name != "pawprint" || c.Apps[0].Catalog != filepath.Join(dir, "catalogs/pawprint.yaml") {
		t.Errorf("app: %+v", c.Apps[0])
	}
	if c.DBPath() != filepath.Join(dir, "data", "hiwikinsight.db") {
		t.Errorf("db path: %s", c.DBPath())
	}
}

func TestValidation(t *testing.T) {
	for _, bad := range []string{
		"timezone: Mars/Olympus",
		"language: fr",
		"apps: [{key: Bad Key, bundle_id: x}]",
		"apps: [{key: a, bundle_id: x}, {key: a, bundle_id: y}]",
		"apps: [{key: a}]",
		"apps: [{key: a, bundle_id: x, color: magenta}]",
		"dashboard: {users: [{username: admin, password_hash: plain}]}",
		"unknown_field: 1",
		"retention_days: -5",
		"feishu: {app_id: cli_a, open_id: ou_1}",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := Parse(nil); err != nil {
		t.Errorf("empty config should be valid: %v", err)
	}
	_, err := Parse([]byte("language: fr\ncurrency: dollars"))
	if err == nil || !strings.Contains(err.Error(), "language") || !strings.Contains(err.Error(), "currency") {
		t.Errorf("all problems should be reported at once: %v", err)
	}
}
