// Package device holds the vendor-neutral half of a backup: what a fetch
// returns, the staged error type used to label failures, and the
// post-processing every driver shares. Vendor drivers live in their own
// packages and depend on this one; nothing here knows what a Junos or a
// RouterOS box is.
package device

import (
	"context"
	"errors"
	"regexp"
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

// StageErr tags err with the stage it happened in.
func StageErr(stage string, err error) error { return &Error{Stage: stage, Err: err} }

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
	// Configs maps a config format to its content. Which format names are
	// valid depends on the vendor; see config.Formats.
	Configs map[string]string
	// Hostname as the device reports it, which may differ from the
	// inventory name.
	Hostname string
	Model    string
	// OSVersion is the vendor's software release, e.g. "21.4R3-S4.9" or
	// "7.14.2 (stable)".
	OSVersion string
	// HeaderBlocks are the verbatim chunks of device output the header is
	// built from, in the order they should appear. A driver leaves a block
	// out when the login class may not run the command behind it, or when
	// the platform does not implement it; an empty block is dropped rather
	// than commented in as a blank.
	HeaderBlocks []string
	// LastCommit is when the running config was last committed on the
	// device, zero when the platform has no such concept or it could not be
	// determined.
	LastCommit   time.Time
	LastCommitBy string
	// Duration is how long the fetch took.
	Duration time.Duration
}

// Fetcher is one vendor's transport-level fetch, called with a context that
// already carries the device timeout.
type Fetcher func(ctx context.Context) (*Result, error)

// Run applies the device timeout, runs the driver's fetch and then the
// post-processing every vendor shares: line sanitising, remove_lines and the
// header block.
func Run(ctx context.Context, d *config.Device, fetch Fetcher) (*Result, error) {
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, d.Timeout.Duration())
	defer cancel()

	res, err := fetch(ctx)
	if err != nil {
		return nil, err
	}

	for format, content := range res.Configs {
		res.Configs[format] = Sanitize(content, d.RemoveMatchers())
	}
	if d.HeaderEnabled() {
		res.applyHeader(d)
	}
	res.Duration = time.Since(start)
	return res, nil
}

// applyHeader prefixes the stored configuration with the metadata comment
// block, for the formats where "#" actually is a comment: prepending it to
// Junos' XML rendering would produce a malformed document.
//
// It runs after Sanitize so that remove_lines cannot strip parts of the header
// out of a device's inventory listing.
func (r *Result) applyHeader(d *config.Device) {
	head := r.Header()
	if head == "" {
		return
	}
	for _, format := range config.CommentedFormats(d.Vendor) {
		if content, ok := r.Configs[format]; ok && content != "" {
			r.Configs[format] = head + content
		}
	}
}

// Header renders what the device is, what it runs, and what hardware and
// licences it carries, as comments.
//
// The blocks are device output, commented and concatenated, with nothing
// re-titled: each command already labels its own output, and passing the text
// through unparsed is what keeps a header that no release can silently reshape
// into something misleading — an element the parser does not recognise would
// go missing, whereas raw text cannot.
func (r *Result) Header() string {
	var b strings.Builder
	for _, block := range r.HeaderBlocks {
		b.WriteString(CommentBlock(block))
	}
	return b.String()
}

// CommentBlock prefixes every line of s with "#". Trailing whitespace is
// dropped, so a blank line becomes a bare "#" instead of "# " and does not
// show up as whitespace noise in the repository.
func CommentBlock(s string) string {
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

// Sanitize normalises line endings, drops lines matching the device's
// remove_lines patterns and guarantees a single trailing newline, so that
// cosmetic differences never show up as a git commit.
func Sanitize(s string, remove []*regexp.Regexp) string {
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
