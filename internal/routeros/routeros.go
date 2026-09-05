// Package routeros fetches configuration and metadata from MikroTik RouterOS
// devices over the SSH CLI. RouterOS has no NETCONF and no configuration
// database to read from: the backup is what `/export` renders.
package routeros

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/device"
	"github.com/didww/jconfig/internal/sshx"
)

// consoleFlags are appended to the login name. "c" turns off console colours
// and "t" turns off terminal capability detection; without them RouterOS
// wraps the configuration in escape sequences even for a non-interactive exec
// request. RouterOS strips everything after "+" before authenticating, so the
// account itself is unchanged.
const consoleFlags = "+ct"

// Fetch retrieves the configuration from a RouterOS device.
func Fetch(ctx context.Context, d *config.Device) (*device.Result, error) {
	return device.Run(ctx, d, func(ctx context.Context) (*device.Result, error) {
		return fetch(ctx, d)
	})
}

func fetch(ctx context.Context, d *config.Device) (*device.Result, error) {
	conn, err := sshx.Dial(ctx, d, login(d.Username))
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	res := &device.Result{Configs: make(map[string]string, len(d.Formats))}

	for _, format := range d.Formats {
		cmd := exportCommand(format, d.SensitiveShown())
		out, err := conn.Run(ctx, cmd)
		if err != nil {
			return nil, device.StageErr(device.StageFetch, err)
		}
		content, err := parseExport(out)
		if err != nil {
			return nil, device.StageErr(device.StageParse, fmt.Errorf("%q: %w", cmd, err))
		}
		if strings.TrimSpace(content) == "" {
			return nil, device.StageErr(device.StageParse, fmt.Errorf("%q: device returned an empty configuration", cmd))
		}
		res.Configs[format] = content
	}

	applyMetadata(ctx, conn, res)
	return res, nil
}

// login returns the user name to authenticate with, carrying the console
// flags. A name that already sets its own flags is left alone.
func login(user string) string {
	if strings.Contains(user, "+") {
		return user
	}
	return user + consoleFlags
}

// exportCommand renders one format. "verbose" and "terse" are RouterOS' own
// flag names; the unflagged export is the compact one, which is what RouterOS
// itself defaults to.
//
// show-sensitive is what makes the export restorable: without it RouterOS
// prints secrets as placeholders. It needs the "sensitive" policy on the
// login's group, and it puts those secrets in the repository, so it is opt-in.
func exportCommand(format string, sensitive bool) string {
	var b strings.Builder
	b.WriteString("/export")
	switch format {
	case config.FormatVerbose:
		b.WriteString(" verbose")
	case config.FormatTerse:
		b.WriteString(" terse")
	}
	if sensitive {
		b.WriteString(" show-sensitive")
	}
	return b.String()
}

// bannerRE matches the timestamp line RouterOS opens every export with, in
// both the v6 ("# jan/15/2024 10:23:45 by RouterOS 6.49.7") and v7
// ("# 2026-09-05 10:23:45 by RouterOS 7.14.2") renderings.
//
// It is the one line in an export that moves on every single fetch, so leaving
// it in place would commit every device on every run, forever. The release it
// names is not lost: it comes back through the `/system resource print` block
// of the header.
var bannerRE = regexp.MustCompile(`^#\s+\S+\s+\d{1,2}:\d{2}:\d{2}\s+by RouterOS\b`)

func parseExport(out []byte) (string, error) {
	text := stripANSI(string(out))
	if err := cliError(text); err != nil {
		return "", err
	}

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if bannerRE.MatchString(l) {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n"), nil
}

// ansiRE matches the colour and erase sequences RouterOS emits when it thinks
// it is talking to a terminal. consoleFlags should prevent them; stripping
// them anyway costs nothing and keeps a device whose user name was overridden
// from writing escape codes into git.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// cliError catches the errors RouterOS prints on stdout with a zero exit
// status. There is no error prefix to key on the way Junos has "error:", so
// this matches the console's own wording.
func cliError(text string) error {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		switch {
		case strings.HasPrefix(low, "bad command name"),
			strings.HasPrefix(low, "expected end of command"),
			strings.HasPrefix(low, "syntax error"),
			strings.HasPrefix(low, "no such command"),
			strings.HasPrefix(low, "invalid command"):
			return errors.New(line)
		case strings.Contains(low, "not enough permissions"):
			return fmt.Errorf("%s (does the login's group have the read, ssh and — for show_sensitive — sensitive policies?)", line)
		}
		// Only the first line can carry the error; an export starts with
		// its banner or with a configuration path.
		return nil
	}
	return errors.New("device returned no output")
}
