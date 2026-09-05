// Package junos fetches configuration and metadata from Junos devices over
// either the SSH CLI or NETCONF.
package junos

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/device"
)

// Fetch retrieves the configuration from a Junos device using its configured
// transport.
func Fetch(ctx context.Context, d *config.Device) (*device.Result, error) {
	return device.Run(ctx, d, func(ctx context.Context) (*device.Result, error) {
		switch d.Transport {
		case config.TransportNETCONF:
			return fetchNETCONF(ctx, d)
		default:
			return fetchSSH(ctx, d)
		}
	})
}

// header is the four commands the metadata block is built from, in the order
// they appear above the configuration. Each is empty when the login class may
// not run the command, or when the platform does not implement it — a
// standalone MX has no virtual chassis to report.
//
// Hostname, model and release are parsed from the XML rendering of
// `show version` instead, because commit messages and metric labels need them
// as values rather than as text.
var header = []string{
	"show version",
	"show chassis hardware",
	"show system license",
	"show virtual-chassis",
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

func applyVersion(res *device.Result, raw []byte) {
	var si softwareInformation
	if err := decodeFirst(raw, "software-information", &si); err != nil {
		return
	}
	res.Hostname = strings.TrimSpace(si.HostName)
	res.Model = strings.TrimSpace(firstNonEmpty(si.ProductModel, si.ProductName))
	res.OSVersion = strings.TrimSpace(si.JunosVersion)
	if res.OSVersion != "" {
		return
	}
	for _, p := range si.Packages {
		if m := versionRE.FindStringSubmatch(p.Comment); m != nil {
			res.OSVersion = strings.TrimSpace(m[1])
			return
		}
	}
}

func applyCommit(res *device.Result, raw []byte) {
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
