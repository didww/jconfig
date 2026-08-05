package junos

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/junostest"
	"golang.org/x/crypto/ssh"
)

func testDevice(f *junostest.Server, transport string) *config.Device {
	yes := true
	d := &config.Device{
		Name:       "mx1",
		Host:       f.Host(),
		Port:       f.Port(),
		Transport:  transport,
		Username:   junostest.User,
		Password:   junostest.Pass,
		KnownHosts: f.KnownHosts(),
		Formats:    []string{config.FormatText, config.FormatSet},
		Timeout:    config.Duration(10 * time.Second),
		Enabled:    &yes,
	}
	return d
}

func TestFetchBothTransports(t *testing.T) {
	for _, transport := range []string{config.TransportSSH, config.TransportNETCONF} {
		t.Run(transport, func(t *testing.T) {
			f := junostest.Start(t)
			d := testDevice(f, transport)

			res, err := Fetch(context.Background(), d)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}

			if got := res.Configs[config.FormatText]; !strings.Contains(got, "host-name mx1-ams;") {
				t.Errorf("text config missing host-name:\n%s", got)
			}
			if got := res.Configs[config.FormatSet]; !strings.Contains(got, "set system host-name mx1-ams") {
				t.Errorf("set config missing host-name:\n%s", got)
			}
			if res.Hostname != "mx1-ams" {
				t.Errorf("Hostname = %q, want mx1-ams", res.Hostname)
			}
			if res.Model != "mx480" {
				t.Errorf("Model = %q, want mx480", res.Model)
			}
			if res.OSVersion != "21.4R3-S4.9" {
				t.Errorf("OSVersion = %q, want 21.4R3-S4.9", res.OSVersion)
			}
			// The most recent commit is sequence-number 0, not the first
			// element in document order.
			if want := time.Unix(1784894400, 0).UTC(); !res.LastCommit.Equal(want) {
				t.Errorf("LastCommit = %v, want %v", res.LastCommit, want)
			}
			if res.LastCommitBy != "admin" {
				t.Errorf("LastCommitBy = %q, want admin", res.LastCommitBy)
			}
		})
	}
}

func TestFetchXMLFormat(t *testing.T) {
	for _, transport := range []string{config.TransportSSH, config.TransportNETCONF} {
		t.Run(transport, func(t *testing.T) {
			f := junostest.Start(t)
			d := testDevice(f, transport)
			d.Formats = []string{config.FormatXML}

			res, err := Fetch(context.Background(), d)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			got := res.Configs[config.FormatXML]
			if !strings.HasPrefix(got, "<configuration") {
				t.Errorf("xml config should start with <configuration:\n%s", got)
			}
			if !strings.HasSuffix(strings.TrimSpace(got), "</configuration>") {
				t.Errorf("xml config should end with </configuration>:\n%s", got)
			}
			if strings.Contains(got, "rpc-reply") {
				t.Errorf("xml config should not include the rpc-reply envelope:\n%s", got)
			}
		})
	}
}

func TestFetchBadCredentials(t *testing.T) {
	f := junostest.Start(t)
	d := testDevice(f, config.TransportSSH)
	d.Password = "wrong"

	_, err := Fetch(context.Background(), d)
	if err == nil {
		t.Fatal("expected an error")
	}
	if stage := StageOf(err); stage != StageAuth {
		t.Errorf("stage = %q, want %q (err: %v)", stage, StageAuth, err)
	}
}

func TestFetchUnknownHostKey(t *testing.T) {
	f := junostest.Start(t)
	d := testDevice(f, config.TransportSSH)
	// A known_hosts file that does not list this server.
	empty := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.KnownHosts = empty

	_, err := Fetch(context.Background(), d)
	if err == nil {
		t.Fatal("expected host key verification to fail")
	}
	if stage := StageOf(err); stage != StageConnect {
		t.Errorf("stage = %q, want %q (err: %v)", stage, StageConnect, err)
	}
}

func TestFetchInsecureHostKey(t *testing.T) {
	f := junostest.Start(t)
	d := testDevice(f, config.TransportSSH)
	yes := true
	d.KnownHosts = ""
	d.InsecureIgnoreHostKey = &yes

	if _, err := Fetch(context.Background(), d); err != nil {
		t.Fatalf("Fetch with host key check disabled: %v", err)
	}
}

func TestFetchCLIErrorOutput(t *testing.T) {
	f := junostest.Start(t)
	f.SetFailCommand("show configuration | display set")
	d := testDevice(f, config.TransportSSH)

	_, err := Fetch(context.Background(), d)
	if err == nil {
		t.Fatal("expected the CLI error to surface")
	}
	if stage := StageOf(err); stage != StageParse {
		t.Errorf("stage = %q, want %q (err: %v)", stage, StageParse, err)
	}
	if !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("error should quote the device message, got: %v", err)
	}
}

func TestFetchNetconfSubsystemRefused(t *testing.T) {
	f := junostest.Start(t)
	f.SetNoNetconf(true)
	d := testDevice(f, config.TransportNETCONF)

	_, err := Fetch(context.Background(), d)
	if err == nil {
		t.Fatal("expected an error when the subsystem is refused")
	}
	if !strings.Contains(err.Error(), "netconf") {
		t.Errorf("error should mention netconf, got: %v", err)
	}
}

func TestFetchEmptyConfigIsAnError(t *testing.T) {
	f := junostest.Start(t)
	f.SetText("   \n")
	d := testDevice(f, config.TransportSSH)
	d.Formats = []string{config.FormatText}

	_, err := Fetch(context.Background(), d)
	if err == nil {
		t.Fatal("an empty configuration must not be treated as a successful backup")
	}
	if stage := StageOf(err); stage != StageParse {
		t.Errorf("stage = %q, want %q (err: %v)", stage, StageParse, err)
	}
}

func TestFetchTimeout(t *testing.T) {
	f := junostest.Start(t)
	d := testDevice(f, config.TransportSSH)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead

	if _, err := Fetch(ctx, d); err == nil {
		t.Fatal("expected a cancelled context to abort the fetch")
	}
}

func TestSanitize(t *testing.T) {
	remove := []*regexp.Regexp{regexp.MustCompile(`^## Last commit:`)}

	// CRLF is normalised, the matching line is dropped, trailing whitespace
	// and blank lines collapse to exactly one newline.
	got := sanitize("## Last commit: now\r\nversion 21.4;   \r\n\r\n", remove)
	want := "version 21.4;\n"
	if got != want {
		t.Errorf("sanitize() = %q, want %q", got, want)
	}

	if got := sanitize("", nil); got != "" {
		t.Errorf("sanitize(\"\") = %q, want empty", got)
	}
	if got := sanitize("a\nb", nil); got != "a\nb\n" {
		t.Errorf("sanitize should add exactly one trailing newline, got %q", got)
	}
}

func TestRawElement(t *testing.T) {
	doc := []byte(`<rpc-reply><configuration-text>x</configuration-text></rpc-reply>`)
	if _, ok := rawElement(doc, "configuration"); ok {
		t.Error("rawElement matched <configuration-text> when asked for <configuration>")
	}

	doc = []byte(`<rpc-reply><configuration junos:changed="1"><a/></configuration></rpc-reply>`)
	got, ok := rawElement(doc, "configuration")
	if !ok {
		t.Fatal("rawElement did not find <configuration>")
	}
	if got != `<configuration junos:changed="1"><a/></configuration>` {
		t.Errorf("rawElement = %q", got)
	}
}

func TestCheckRPCError(t *testing.T) {
	warn := []byte(`<rpc-reply><rpc-error><error-severity>warning</error-severity>` +
		`<error-message>statement not found</error-message></rpc-error></rpc-reply>`)
	if err := checkRPCError(warn); err != nil {
		t.Errorf("warnings must not fail the backup, got %v", err)
	}

	bad := []byte(`<rpc-reply><rpc-error><error-severity>error</error-severity>` +
		`<error-message>permission denied</error-message></rpc-error></rpc-reply>`)
	if err := checkRPCError(bad); err == nil {
		t.Error("expected an error for severity=error")
	}
}

// testKey is a throwaway ed25519 private key in OpenSSH PEM form.
func testKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

// A key can be given inline instead of as a file, either as PEM or, for
// passing through a single-line environment variable, base64.
func TestFetchWithInlineKey(t *testing.T) {
	key := testKey(t)

	for _, tc := range []struct {
		name  string
		value string
	}{
		{"pem", key},
		{"base64", base64.StdEncoding.EncodeToString([]byte(key))},
		{"base64 with newlines", wrap(base64.StdEncoding.EncodeToString([]byte(key)), 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := junostest.Start(t)
			d := testDevice(f, config.TransportSSH)
			d.Password = "" // key only
			d.Key = tc.value

			res, err := Fetch(context.Background(), d)
			if err != nil {
				t.Fatalf("Fetch with an inline key: %v", err)
			}
			if !strings.Contains(res.Configs[config.FormatText], "host-name mx1-ams;") {
				t.Error("did not retrieve the configuration")
			}
		})
	}
}

func TestFetchWithKeyFile(t *testing.T) {
	f := junostest.Start(t)
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, []byte(testKey(t)), 0o600); err != nil {
		t.Fatal(err)
	}

	d := testDevice(f, config.TransportSSH)
	d.Password = ""
	d.KeyFile = path

	if _, err := Fetch(context.Background(), d); err != nil {
		t.Fatalf("Fetch with a key file: %v", err)
	}
}

func TestFetchInlineKeyInvalid(t *testing.T) {
	f := junostest.Start(t)
	d := testDevice(f, config.TransportSSH)
	d.Password = ""
	d.Key = "bm90LWEta2V5" // base64 of "not-a-key"

	_, err := Fetch(context.Background(), d)
	if err == nil {
		t.Fatal("expected an invalid inline key to fail")
	}
	if stage := StageOf(err); stage != StageAuth {
		t.Errorf("stage = %q, want %q (err: %v)", stage, StageAuth, err)
	}
}

func TestFetchHeader(t *testing.T) {
	for _, transport := range []string{config.TransportSSH, config.TransportNETCONF} {
		t.Run(transport, func(t *testing.T) {
			f := junostest.Start(t)
			d := testDevice(f, transport)
			d.Formats = []string{config.FormatText, config.FormatSet, config.FormatXML}

			res, err := Fetch(context.Background(), d)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}

			for _, format := range []string{config.FormatText, config.FormatSet} {
				got := res.Configs[format]
				for _, want := range []string{
					"# Hostname: mx1-ams\n",
					"# Model: mx480\n",
					"# Junos: 21.4R3-S4.9\n",
					"# JUNOS Software Release [21.4R3-S4.9]\n",
					"# Hardware inventory:\n",
					"# Chassis                                JN123456AB        MX480\n",
					"# License usage:\n",
					"# Licenses installed: none\n",
				} {
					if !strings.Contains(got, want) {
						t.Errorf("%s config missing %q:\n%s", format, want, got)
					}
				}
				if !strings.HasPrefix(got, "# Hostname: mx1-ams\n") {
					t.Errorf("%s config should open with the header:\n%s", format, got)
				}
				// The configuration itself must survive underneath it.
				if !strings.Contains(got, "host-name mx1-ams") {
					t.Errorf("%s config missing the configuration:\n%s", format, got)
				}
				// A blank line inside the device output is commented as a
				// bare "#", never "# ".
				for _, l := range strings.Split(got, "\n") {
					if l != strings.TrimRight(l, " \t") {
						t.Errorf("%s config has a line with trailing whitespace: %q", format, l)
					}
				}
			}

			// "#" is not a comment in XML, so that rendering is left alone.
			if got := res.Configs[config.FormatXML]; !strings.HasPrefix(got, "<configuration") {
				t.Errorf("xml config should not be prefixed with the header:\n%s", got)
			}
		})
	}
}

func TestFetchHeaderDisabled(t *testing.T) {
	f := junostest.Start(t)
	d := testDevice(f, config.TransportSSH)
	no := false
	d.Header = &no

	res, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := res.Configs[config.FormatText]; strings.Contains(got, "# Hostname:") {
		t.Errorf("header: false should leave the config untouched:\n%s", got)
	}
	// The metadata is still collected, it is just not written out.
	if res.Inventory == "" {
		t.Error("Inventory should still be populated")
	}
}

// wrap breaks s into lines of n characters, the way base64 often arrives.
func wrap(s string, n int) string {
	var b strings.Builder
	for len(s) > n {
		b.WriteString(s[:n])
		b.WriteByte('\n')
		s = s[n:]
	}
	b.WriteString(s)
	return b.String()
}
