package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// write drops a file into dir and returns its path.
func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "jconfig.yml", `
repo:
  path: /var/lib/jconfig/configs
defaults:
  username: backup
  password: pw
  insecure_ignore_host_key: true
devices:
  - name: mx1
    host: 10.0.0.1
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Listen != ":9639" {
		t.Errorf("Listen = %q, want :9639", cfg.Listen)
	}
	if cfg.Repo.Branch != "main" {
		t.Errorf("Repo.Branch = %q, want main", cfg.Repo.Branch)
	}
	if cfg.Scheduler.Interval.Duration() != time.Hour {
		t.Errorf("Scheduler.Interval = %v, want 1h", cfg.Scheduler.Interval)
	}
	if cfg.Scheduler.Concurrency != 8 {
		t.Errorf("Concurrency = %d, want 8", cfg.Scheduler.Concurrency)
	}

	d := cfg.Devices[0]
	if d.Transport != TransportSSH {
		t.Errorf("Transport = %q, want ssh", d.Transport)
	}
	if d.Port != 22 {
		t.Errorf("Port = %d, want 22", d.Port)
	}
	if got := strings.Join(d.Formats, ","); got != "text,set" {
		t.Errorf("Formats = %q, want text,set", got)
	}
	if d.Username != "backup" {
		t.Errorf("Username = %q, want backup (from defaults)", d.Username)
	}
	if d.Interval.Duration() != time.Hour {
		t.Errorf("device Interval = %v, want the scheduler default", d.Interval)
	}
	if !d.IsEnabled() {
		t.Error("device should be enabled by default")
	}
}

func TestNetconfDefaultsToPort830(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "jconfig.yml", `
repo:
  path: /tmp/repo
defaults:
  username: backup
  password: pw
  insecure_ignore_host_key: true
devices:
  - name: mx1
    host: 10.0.0.1
    transport: netconf
  - name: mx2
    host: 10.0.0.2
    transport: netconf
    port: 22
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Devices[0].Port != 830 {
		t.Errorf("netconf default port = %d, want 830", cfg.Devices[0].Port)
	}
	if cfg.Devices[1].Port != 22 {
		t.Errorf("explicit port should win, got %d", cfg.Devices[1].Port)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("JCONFIG_TEST_PW", "from-env")

	dir := t.TempDir()
	path := write(t, dir, "jconfig.yml", `
repo:
  path: /tmp/repo
defaults:
  username: ${JCONFIG_TEST_USER:-backup}
  password: ${JCONFIG_TEST_PW}
  insecure_ignore_host_key: true
devices:
  - name: mx1
    host: 10.0.0.1
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Devices[0].Password; got != "from-env" {
		t.Errorf("Password = %q, want from-env", got)
	}
	if got := cfg.Devices[0].Username; got != "backup" {
		t.Errorf("Username = %q, want the :- default", got)
	}
}

func TestEnvEscape(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "jconfig.yml", `
repo:
  path: /tmp/repo
defaults:
  username: backup
  password: "$${NOT_AN_ENV_VAR}"
  insecure_ignore_host_key: true
devices:
  - name: mx1
    host: 10.0.0.1
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Devices[0].Password; got != "${NOT_AN_ENV_VAR}" {
		t.Errorf("Password = %q, want the literal ${NOT_AN_ENV_VAR}", got)
	}
}

func TestValidation(t *testing.T) {
	base := `
repo:
  path: /tmp/repo
defaults:
  username: backup
  password: pw
  insecure_ignore_host_key: true
`
	tests := []struct {
		name    string
		devices string
		want    string
	}{
		{"duplicate names", `
devices:
  - name: mx1
    host: 10.0.0.1
  - name: mx1
    host: 10.0.0.2
`, "duplicate device name"},
		{"missing host", `
devices:
  - name: mx1
`, "host is required"},
		{"bad transport", `
devices:
  - name: mx1
    host: 10.0.0.1
    transport: telnet
`, `transport "telnet"`},
		{"bad format", `
devices:
  - name: mx1
    host: 10.0.0.1
    formats: [text, json]
`, `unknown format "json"`},
		{"bad regex", `
devices:
  - name: mx1
    host: 10.0.0.1
    remove_lines: ["[unclosed"]
`, "remove_lines"},
		{"no devices", "devices: []", "no devices configured"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := write(t, dir, "jconfig.yml", base+tc.devices)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestMissingCredentialsRejected(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "jconfig.yml", `
repo:
  path: /tmp/repo
devices:
  - name: mx1
    host: 10.0.0.1
    username: backup
    insecure_ignore_host_key: true
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("expected a credentials error, got %v", err)
	}
}

func TestMissingKnownHostsRejected(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "jconfig.yml", `
repo:
  path: /tmp/repo
devices:
  - name: mx1
    host: 10.0.0.1
    username: backup
    password: pw
    known_hosts: /nonexistent/known_hosts
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("expected a known_hosts error, got %v", err)
	}
	if !strings.Contains(err.Error(), "insecure_ignore_host_key") {
		t.Errorf("the error should say how to opt out, got %v", err)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "jconfig.yml", `
repo:
  path: /tmp/repo
scheduler:
  intrval: 5m
devices:
  - name: mx1
    host: 10.0.0.1
    username: backup
    password: pw
    insecure_ignore_host_key: true
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "intrval") {
		t.Fatalf("a typo in a field name should be reported, got %v", err)
	}
}

func TestEnabledDevices(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "jconfig.yml", `
repo:
  path: /tmp/repo
defaults:
  username: backup
  password: pw
  insecure_ignore_host_key: true
devices:
  - name: mx1
    host: 10.0.0.1
  - name: mx2
    host: 10.0.0.2
    enabled: false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	enabled := cfg.EnabledDevices()
	if len(enabled) != 1 || enabled[0].Name != "mx1" {
		t.Errorf("EnabledDevices() = %v, want just mx1", enabled)
	}
}

func TestDurationParsing(t *testing.T) {
	var v struct {
		A Duration `yaml:"a"`
		B Duration `yaml:"b"`
	}
	if err := yaml.Unmarshal([]byte("a: 15m\nb: 90\n"), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.A.Duration() != 15*time.Minute {
		t.Errorf("a = %v, want 15m", v.A.Duration())
	}
	// A bare number is read as seconds.
	if v.B.Duration() != 90*time.Second {
		t.Errorf("b = %v, want 90s", v.B.Duration())
	}
	if err := yaml.Unmarshal([]byte("a: nonsense\n"), &v); err == nil {
		t.Error("expected an error for an invalid duration")
	}
}

func TestDecodeKey(t *testing.T) {
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"

	// A literal PEM block is returned as-is, with a trailing newline.
	got, err := DecodeKey(key)
	if err != nil {
		t.Fatalf("DecodeKey(pem): %v", err)
	}
	if string(got) != key+"\n" {
		t.Errorf("DecodeKey(pem) = %q", got)
	}

	// Base64 is decoded.
	got, err = DecodeKey(base64.StdEncoding.EncodeToString([]byte(key)))
	if err != nil {
		t.Fatalf("DecodeKey(base64): %v", err)
	}
	if string(got) != key+"\n" {
		t.Errorf("DecodeKey(base64) = %q", got)
	}

	// Base64 split across lines, as secret stores often render it.
	b64 := base64.StdEncoding.EncodeToString([]byte(key))
	if _, err := DecodeKey(b64[:10] + "\n" + b64[10:]); err != nil {
		t.Errorf("DecodeKey(wrapped base64): %v", err)
	}

	for _, bad := range []string{"", "   ", "not base64 !!!", "aGVsbG8="} {
		if _, err := DecodeKey(bad); err == nil {
			t.Errorf("DecodeKey(%q) should have failed", bad)
		}
	}
}

func TestInlineKeyValidation(t *testing.T) {
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"

	dir := t.TempDir()
	path := write(t, dir, "jconfig.yml", `
repo:
  path: /tmp/repo
devices:
  - name: mx1
    host: 10.0.0.1
    username: backup
    insecure_ignore_host_key: true
    key: |
`+indent(key, 6))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("an inline key should satisfy the credentials check: %v", err)
	}
	if cfg.Devices[0].Key == "" {
		t.Error("inline key was not loaded")
	}

	// key and key_file together are ambiguous.
	path = write(t, dir, "both.yml", `
repo:
  path: /tmp/repo
devices:
  - name: mx1
    host: 10.0.0.1
    username: backup
    insecure_ignore_host_key: true
    key_file: /etc/ssh/nope
    key: |
`+indent(key, 6))
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("expected a key/key_file conflict error, got %v", err)
	}

	// A malformed inline key is caught at load time, not at connect time.
	path = write(t, dir, "bad.yml", `
repo:
  path: /tmp/repo
devices:
  - name: mx1
    host: 10.0.0.1
    username: backup
    insecure_ignore_host_key: true
    key: "@@@not-a-key@@@"
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "key:") {
		t.Errorf("expected an invalid key error, got %v", err)
	}
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		out = append(out, pad+l)
	}
	return strings.Join(out, "\n")
}
