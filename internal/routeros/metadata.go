package routeros

import (
	"context"
	"regexp"
	"strings"

	"github.com/didww/jconfig/internal/device"
	"github.com/didww/jconfig/internal/sshx"
)

// headerCommands are the `print` commands the metadata block is built from,
// in the order they appear above the configuration, each with the keys that
// are kept.
//
// Junos' header is raw command output, on the grounds that text no parser
// touched cannot be silently reshaped into something misleading. RouterOS does
// not allow that: `/system resource print` reports uptime, free memory and CPU
// load beside the board and release, and a header carrying those would change
// on every single fetch and commit every device, every run. So the keys are
// listed explicitly, and the lines are still the device's own — filtered, not
// reformatted or re-titled.
//
// The cost of the list is that a key RouterOS renames goes missing instead of
// showing up under its new name. That is the trade for a header that only
// moves when the hardware or the software does.
var headerCommands = []struct {
	cmd  string
	keys []string
}{
	{"/system resource print", []string{
		"version", "factory-software", "build-time",
		"board-name", "platform", "architecture-name",
		"cpu", "cpu-count", "total-memory", "total-hdd-space",
	}},
	{"/system routerboard print", []string{
		"routerboard", "model", "revision", "serial-number",
		"firmware-type", "factory-firmware", "current-firmware",
		// upgrade-firmware is deliberately absent: it tracks what
		// MikroTik has released, so it moves without the device changing.
	}},
	{"/system license print", []string{
		"software-id", "system-id", "nlevel", "level", "features",
		"limit-upgrade-to",
	}},
}

// applyMetadata fills in the header blocks and the reported identity. It is
// best effort throughout: a device that will not report its board is still
// worth backing up, and a group without the permission to read one of these
// menus should lose that block, not the backup.
//
// LastCommit and LastCommitBy stay zero. RouterOS has no commit history to
// read: `/system history` is an undo journal that records no author and rolls
// over, so reporting it as a commit time would be inventing a fact.
func applyMetadata(ctx context.Context, conn *sshx.Conn, res *device.Result) {
	raw := make(map[string]string, len(headerCommands))

	for _, h := range headerCommands {
		out, err := conn.Run(ctx, h.cmd)
		if err != nil {
			res.HeaderBlocks = append(res.HeaderBlocks, "")
			continue
		}
		text := stripANSI(string(out))
		if cliError(text) != nil {
			res.HeaderBlocks = append(res.HeaderBlocks, "")
			continue
		}
		raw[h.cmd] = text
		res.HeaderBlocks = append(res.HeaderBlocks, filterKeys(text, h.keys))
	}

	resource := parseKeys(raw["/system resource print"])
	board := parseKeys(raw["/system routerboard print"])

	res.OSVersion = resource["version"]
	// A RouterBOARD names itself twice; the routerboard menu is the more
	// specific of the two and is absent on CHR and x86.
	res.Model = firstNonEmpty(board["model"], resource["board-name"], resource["platform"])

	if out, err := conn.Run(ctx, "/system identity print"); err == nil {
		text := stripANSI(string(out))
		if cliError(text) == nil {
			res.Hostname = parseKeys(text)["name"]
		}
	}
}

// keyRE matches one "  key: value" line of RouterOS `print` output. A line
// that does not match is a continuation of the value above it, which is how
// long lists such as the licence features wrap.
var keyRE = regexp.MustCompile(`^\s*([a-z0-9-]+):\s?(.*)$`)

// filterKeys keeps the lines of print output whose key is listed, verbatim,
// continuations included. The device's own alignment is preserved: it is
// computed from the widest key in the full output, so dropping lines does not
// shift the ones that stay.
func filterKeys(text string, keys []string) string {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}

	var (
		out  []string
		keep bool
	)
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if m := keyRE.FindStringSubmatch(l); m != nil {
			keep = want[m[1]]
		} else if strings.TrimSpace(l) == "" {
			keep = false
		}
		if keep {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// parseKeys reads print output into a key/value map, for the fields that are
// wanted as values rather than as text.
func parseKeys(text string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if m := keyRE.FindStringSubmatch(l); m != nil {
			out[m[1]] = strings.TrimSpace(m[2])
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
