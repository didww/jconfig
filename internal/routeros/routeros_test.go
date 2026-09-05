package routeros

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/device"
	"github.com/didww/jconfig/internal/rostest"
)

func testDevice(f *rostest.Server) *config.Device {
	yes := true
	return &config.Device{
		Name:       "gw1",
		Vendor:     config.VendorRouterOS,
		Host:       f.Host(),
		Port:       f.Port(),
		Transport:  config.TransportSSH,
		Username:   rostest.User,
		Password:   rostest.Pass,
		KnownHosts: f.KnownHosts(),
		Formats:    []string{config.FormatExport},
		Timeout:    config.Duration(10 * time.Second),
		Enabled:    &yes,
	}
}

func TestFetch(t *testing.T) {
	f := rostest.Start(t)
	d := testDevice(f)

	res, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	got := res.Configs[config.FormatExport]
	if !strings.Contains(got, "/system identity\nset name=gw1-ams") {
		t.Errorf("export missing the configuration:\n%s", got)
	}
	if res.Hostname != "gw1-ams" {
		t.Errorf("Hostname = %q, want gw1-ams", res.Hostname)
	}
	if res.Model != "RB5009UG+S+" {
		t.Errorf("Model = %q, want RB5009UG+S+", res.Model)
	}
	if res.OSVersion != "7.14.2 (stable)" {
		t.Errorf("OSVersion = %q, want \"7.14.2 (stable)\"", res.OSVersion)
	}
	// RouterOS has no commit history, so there is nothing to report.
	if !res.LastCommit.IsZero() || res.LastCommitBy != "" {
		t.Errorf("LastCommit = %v/%q, want zero for RouterOS", res.LastCommit, res.LastCommitBy)
	}
}

func TestFetchAllFormats(t *testing.T) {
	f := rostest.Start(t)
	d := testDevice(f)
	d.Formats = []string{config.FormatExport, config.FormatVerbose, config.FormatTerse}

	res, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Each format is the rendering RouterOS was actually asked for.
	if got := res.Configs[config.FormatVerbose]; !strings.Contains(got, "ageing-time=5m") {
		t.Errorf("verbose export missing the defaults:\n%s", got)
	}
	if got := res.Configs[config.FormatTerse]; !strings.Contains(got, "/ip address add address=192.0.2.1/24") {
		t.Errorf("terse export not in one-line form:\n%s", got)
	}
}

// The timestamp line changes on every fetch. Left in, it would commit every
// device on every run.
func TestBannerIsStripped(t *testing.T) {
	f := rostest.Start(t)
	d := testDevice(f)

	res, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := res.Configs[config.FormatExport]
	if strings.Contains(got, "by RouterOS") {
		t.Errorf("the export banner survived:\n%s", got)
	}
	// The comment lines below it are stable and must stay.
	for _, want := range []string{"# model = RB5009UG+S+", "# serial number = 00000000TEST"} {
		if !strings.Contains(got, want) {
			t.Errorf("export missing %q:\n%s", want, got)
		}
	}
}

// Two fetches of an unchanged device have to produce identical bytes, or the
// store commits on every run.
func TestFetchIsStable(t *testing.T) {
	f := rostest.Start(t)
	d := testDevice(f)

	first, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	second, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if first.Configs[config.FormatExport] != second.Configs[config.FormatExport] {
		t.Errorf("two fetches differ:\n--- first ---\n%s\n--- second ---\n%s",
			first.Configs[config.FormatExport], second.Configs[config.FormatExport])
	}
}

func TestShowSensitive(t *testing.T) {
	f := rostest.Start(t)
	d := testDevice(f)

	// Off by default: RouterOS masks the key.
	res, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.Contains(res.Configs[config.FormatExport], `private-key="0000`) {
		t.Error("secrets were exported without show_sensitive")
	}

	yes := true
	d.ShowSensitive = &yes
	res, err = Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch with show_sensitive: %v", err)
	}
	if !strings.Contains(res.Configs[config.FormatExport], `private-key="0000`) {
		t.Errorf("show_sensitive did not reach the device:\n%s", res.Configs[config.FormatExport])
	}
}

// The console flags have to be offered at authentication time, or RouterOS
// answers with escape sequences around the configuration.
func TestConsoleFlagsInLogin(t *testing.T) {
	f := rostest.Start(t)
	d := testDevice(f)

	if _, err := Fetch(context.Background(), d); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	logins := f.Logins()
	if len(logins) == 0 {
		t.Fatal("the server saw no login")
	}
	for _, l := range logins {
		if l != rostest.User+"+ct" {
			t.Errorf("login = %q, want %q", l, rostest.User+"+ct")
		}
	}
}

func TestLoginKeepsExplicitFlags(t *testing.T) {
	if got := login("backup+cte80w"); got != "backup+cte80w" {
		t.Errorf("login() = %q, want the configured flags untouched", got)
	}
	if got := login("backup"); got != "backup+ct" {
		t.Errorf("login() = %q, want backup+ct", got)
	}
}

// The header keeps the fields that identify the box and drops the counters
// that move between reads.
func TestHeader(t *testing.T) {
	f := rostest.Start(t)
	d := testDevice(f)

	res, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := res.Configs[config.FormatExport]

	for _, want := range []string{
		"version: 7.14.2 (stable)",
		"board-name: RB5009UG+S+",
		"serial-number: 00000000TEST",
		"current-firmware: 7.14.2",
		"software-id: TEST-0000",
	} {
		if !strings.Contains(got, "# ") || !strings.Contains(got, want) {
			t.Errorf("header missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"uptime:", "free-memory:", "cpu-load:", "free-hdd-space:",
		"write-sect-since-reboot:", "bad-blocks:",
		// Tracks what MikroTik has released, not what this device runs.
		"upgrade-firmware:",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("volatile field %q reached the header:\n%s", unwanted, got)
		}
	}
	if !strings.HasPrefix(got, "# ") {
		t.Errorf("the export should open with the header:\n%s", got)
	}
}

func TestHeaderDisabled(t *testing.T) {
	f := rostest.Start(t)
	d := testDevice(f)
	no := false
	d.Header = &no

	res, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.Contains(res.Configs[config.FormatExport], "board-name:") {
		t.Error("header: false should leave the export untouched")
	}
	// The metadata is still collected, it is just not written out.
	if res.Model == "" {
		t.Error("Model should still be populated")
	}
}

// A group that may export but not read a menu loses that block, not the
// backup.
func TestHeaderCommandRefused(t *testing.T) {
	f := rostest.Start(t)
	f.SetFailCommand("/system license print")
	d := testDevice(f)

	res, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := res.Configs[config.FormatExport]
	if strings.Contains(got, "bad command name") {
		t.Errorf("the device's error leaked into the header:\n%s", got)
	}
	if !strings.Contains(got, "board-name: RB5009UG+S+") {
		t.Errorf("the blocks that did answer are missing:\n%s", got)
	}
}

// RouterOS reports a bad command on stdout with a zero exit status, so the
// export has to be inspected rather than trusted.
func TestExportErrorIsNotABackup(t *testing.T) {
	f := rostest.Start(t)
	f.SetFailCommand("/export")
	d := testDevice(f)

	_, err := Fetch(context.Background(), d)
	if err == nil {
		t.Fatal("a RouterOS error must not be stored as a configuration")
	}
	if stage := device.StageOf(err); stage != device.StageParse {
		t.Errorf("stage = %q, want %q (err: %v)", stage, device.StageParse, err)
	}
}

func TestEmptyExportIsAnError(t *testing.T) {
	f := rostest.Start(t)
	f.SetExport("   \n")
	d := testDevice(f)

	_, err := Fetch(context.Background(), d)
	if err == nil {
		t.Fatal("an empty export must not be treated as a successful backup")
	}
	if stage := device.StageOf(err); stage != device.StageParse {
		t.Errorf("stage = %q, want %q (err: %v)", stage, device.StageParse, err)
	}
}

func TestFetchBadCredentials(t *testing.T) {
	f := rostest.Start(t)
	d := testDevice(f)
	d.Password = "wrong"

	_, err := Fetch(context.Background(), d)
	if err == nil {
		t.Fatal("expected an error")
	}
	if stage := device.StageOf(err); stage != device.StageAuth {
		t.Errorf("stage = %q, want %q (err: %v)", stage, device.StageAuth, err)
	}
}

func TestExportCommand(t *testing.T) {
	cases := []struct {
		format    string
		sensitive bool
		want      string
	}{
		{config.FormatExport, false, "/export"},
		{config.FormatExport, true, "/export show-sensitive"},
		{config.FormatVerbose, false, "/export verbose"},
		{config.FormatTerse, true, "/export terse show-sensitive"},
	}
	for _, tc := range cases {
		if got := exportCommand(tc.format, tc.sensitive); got != tc.want {
			t.Errorf("exportCommand(%q, %v) = %q, want %q", tc.format, tc.sensitive, got, tc.want)
		}
	}
}

// Escape sequences must never reach the repository, even if a device answers
// with them despite the console flags.
func TestANSIIsStripped(t *testing.T) {
	f := rostest.Start(t)
	f.SetExport("\x1b[36m/system identity\x1b[m\nset name=gw1-ams\n")
	d := testDevice(f)

	res, err := Fetch(context.Background(), d)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.Contains(res.Configs[config.FormatExport], "\x1b") {
		t.Error("escape sequences reached the stored configuration")
	}
}

func TestFilterKeysKeepsContinuations(t *testing.T) {
	in := "  software-id: TEST-0000\n" +
		"     features: one,\n" +
		"               two\n" +
		"        nlevel: 4\n" +
		"        uptime: 3w4d\n"
	got := filterKeys(in, []string{"software-id", "features"})
	want := "  software-id: TEST-0000\n" +
		"     features: one,\n" +
		"               two"
	if got != want {
		t.Errorf("filterKeys() = %q, want %q", got, want)
	}
}
