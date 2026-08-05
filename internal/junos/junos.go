// Package junos fetches configuration and metadata from Junos devices over
// either the SSH CLI or NETCONF.
package junos

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/didww/jconfig/internal/config"
)

// Stages of a backup, used as the `stage` label on error metrics.
const (
	StageConnect = "connect"
	StageAuth    = "auth"
	StageFetch   = "fetch"
	StageParse   = "parse"
)

// Error carries the stage a failure happened in so it can be labelled. The
// stage is not repeated in the message: callers report it separately, as a
// metric label or a log field.
type Error struct {
	Stage string
	Err   error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func stageErr(stage string, err error) error { return &Error{Stage: stage, Err: err} }

// StageOf returns the stage of err, or StageFetch when it is not a staged error.
func StageOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Stage
	}
	return StageFetch
}

// Result is everything a single backup run collected from a device.
type Result struct {
	// Configs maps a config format ("text", "set", "xml") to its content.
	Configs map[string]string
	// Hostname as the device reports it, which may differ from the
	// inventory name.
	Hostname string
	Model    string
	// OSVersion is the Junos release, e.g. "21.4R3-S4.9".
	OSVersion string
	// SoftwareRelease is the release banner as the device words it, e.g.
	// "JUNOS Software Release [21.4R3-S4.9]".
	SoftwareRelease string
	// Inventory and Licenses are the verbatim CLI output of
	// `show chassis hardware` and `show system license`. Both are empty when
	// the login class may not run them.
	Inventory string
	Licenses  string
	// LastCommit is when the running config was last committed on the
	// device, zero if it could not be determined.
	LastCommit   time.Time
	LastCommitBy string
	// Duration is how long the fetch took.
	Duration time.Duration
}

// Fetch retrieves the configuration from a device using its configured
// transport.
func Fetch(ctx context.Context, d *config.Device) (*Result, error) {
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, d.Timeout.Duration())
	defer cancel()

	var (
		res *Result
		err error
	)
	switch d.Transport {
	case config.TransportNETCONF:
		res, err = fetchNETCONF(ctx, d)
	case config.TransportSSH:
		res, err = fetchSSH(ctx, d)
	default:
		return nil, stageErr(StageConnect, fmt.Errorf("unknown transport %q", d.Transport))
	}
	if err != nil {
		return nil, err
	}

	for format, content := range res.Configs {
		res.Configs[format] = sanitize(content, d.RemoveMatchers())
	}
	if d.HeaderEnabled() {
		res.applyHeader()
	}
	res.Duration = time.Since(start)
	return res, nil
}

// applyHeader prefixes the stored configuration with the metadata comment
// block. Only the text and set forms get it: "#" is a comment in both, while
// prepending it to the XML rendering would produce a malformed document.
//
// It runs after sanitize so that remove_lines cannot strip parts of the header
// out of a device's inventory listing.
func (r *Result) applyHeader() {
	head := r.header()
	if head == "" {
		return
	}
	for _, format := range []string{config.FormatText, config.FormatSet} {
		if content, ok := r.Configs[format]; ok && content != "" {
			r.Configs[format] = head + content
		}
	}
}

// header renders what the device is, what it runs, and what hardware and
// licences it carries, as Junos comments. `show chassis hardware` and
// `show system license` already label their own output ("Hardware inventory:",
// "License usage:"), so it is passed through verbatim rather than re-titled.
func (r *Result) header() string {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}
	if r.Hostname != "" {
		line("# Hostname: %s", r.Hostname)
	}
	if r.Model != "" {
		line("# Model: %s", r.Model)
	}
	if r.OSVersion != "" {
		line("# Junos: %s", r.OSVersion)
	}
	if r.SoftwareRelease != "" {
		line("# %s", r.SoftwareRelease)
	}
	b.WriteString(commentBlock(r.Inventory))
	b.WriteString(commentBlock(r.Licenses))
	return b.String()
}

// commentBlock prefixes every line of s with "#". Trailing whitespace is
// dropped, so a blank line becomes a bare "#" instead of "# " and does not
// show up as whitespace noise in the repository.
func commentBlock(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		b.WriteString(strings.TrimRight("# "+l, " \t"))
		b.WriteByte('\n')
	}
	return b.String()
}

// sanitize normalises line endings, drops lines matching the device's
// remove_lines patterns and guarantees a single trailing newline, so that
// cosmetic differences never show up as a git commit.
func sanitize(s string, remove []*regexp.Regexp) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
line:
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		for _, re := range remove {
			if re.MatchString(l) {
				continue line
			}
		}
		out = append(out, l)
	}

	joined := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if joined == "" {
		return ""
	}
	return joined + "\n"
}

// --- metadata parsing, shared by both transports ---

type softwareInformation struct {
	HostName     string `xml:"host-name"`
	ProductModel string `xml:"product-model"`
	ProductName  string `xml:"product-name"`
	JunosVersion string `xml:"junos-version"`
	Packages     []struct {
		Name    string `xml:"name"`
		Comment string `xml:"comment"`
	} `xml:"package-information"`
}

type commitInformation struct {
	History []struct {
		SequenceNumber int    `xml:"sequence-number"`
		User           string `xml:"user"`
		Client         string `xml:"client"`
		DateTime       struct {
			Seconds string `xml:"seconds,attr"`
			Value   string `xml:",chardata"`
		} `xml:"date-time"`
	} `xml:"commit-history"`
}

// versionRE pulls a release out of a package comment such as
// "JUNOS Base OS boot [21.4R3-S4.9]" on releases that omit <junos-version>.
var versionRE = regexp.MustCompile(`\[([^\]]+)\]`)

// releaseRE picks the package comment that carries the release banner, e.g.
// "JUNOS Software Release [21.4R3-S4.9]", out of the dozens a device lists.
var releaseRE = regexp.MustCompile(`(?i)software release \[`)

func applyVersion(res *Result, raw []byte) {
	var si softwareInformation
	if err := decodeFirst(raw, "software-information", &si); err != nil {
		return
	}
	res.Hostname = strings.TrimSpace(si.HostName)
	res.Model = strings.TrimSpace(firstNonEmpty(si.ProductModel, si.ProductName))
	res.OSVersion = strings.TrimSpace(si.JunosVersion)
	for _, p := range si.Packages {
		comment := strings.TrimSpace(p.Comment)
		if res.SoftwareRelease == "" && releaseRE.MatchString(comment) {
			res.SoftwareRelease = comment
		}
		if res.OSVersion == "" {
			if m := versionRE.FindStringSubmatch(comment); m != nil {
				res.OSVersion = strings.TrimSpace(m[1])
			}
		}
	}
}

func applyCommit(res *Result, raw []byte) {
	var ci commitInformation
	if err := decodeFirst(raw, "commit-information", &ci); err != nil {
		return
	}
	// Sequence number 0 is the most recent commit; fall back to the first
	// entry if the sequence numbers are missing.
	best := -1
	for i, h := range ci.History {
		if h.SequenceNumber == 0 {
			best = i
			break
		}
		if best < 0 || h.SequenceNumber < ci.History[best].SequenceNumber {
			best = i
		}
	}
	if best < 0 {
		return
	}
	h := ci.History[best]
	res.LastCommitBy = strings.TrimSpace(h.User)
	if secs, err := strconv.ParseInt(strings.TrimSpace(h.DateTime.Seconds), 10, 64); err == nil && secs > 0 {
		res.LastCommit = time.Unix(secs, 0).UTC()
		return
	}
	// Older releases only render the human-readable form.
	if t, err := time.Parse("2006-01-02 15:04:05 MST", strings.TrimSpace(h.DateTime.Value)); err == nil {
		res.LastCommit = t.UTC()
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
