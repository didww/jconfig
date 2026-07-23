package junos

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

var errElementNotFound = errors.New("element not found")

// newDecoder returns a lenient XML decoder. Junos emits
// `<?xml version="1.0" encoding="us-ascii"?>`, which the stdlib refuses
// without a CharsetReader, so pass the bytes through untouched.
func newDecoder(r io.Reader) *xml.Decoder {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	return dec
}

// decodeFirst finds the first element with the given local name anywhere in
// the document and decodes it into v. Matching on the local name sidesteps the
// junos: and namespace prefixes that vary between releases and between the CLI
// and NETCONF renderings of the same RPC.
func decodeFirst(data []byte, local string, v any) error {
	dec := newDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: <%s>", errElementNotFound, local)
		}
		if err != nil {
			return fmt.Errorf("scan xml: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == local {
			return dec.DecodeElement(v, &se)
		}
	}
}

// elementText returns the character data of the first element with the given
// local name, with XML entities decoded.
func elementText(data []byte, local string) (string, error) {
	var s string
	if err := decodeFirst(data, local, &s); err != nil {
		return "", err
	}
	return s, nil
}

// rawElement returns the first element with the given local name verbatim,
// tags included. Used for the XML config format, where re-encoding through the
// stdlib would reorder attributes and churn the diff.
func rawElement(data []byte, local string) (string, bool) {
	s := string(data)
	start := -1
	for i := 0; ; {
		idx := strings.Index(s[i:], "<"+local)
		if idx < 0 {
			return "", false
		}
		idx += i
		// Reject prefixed matches (<configuration-text> when looking for
		// <configuration>) by checking the character that follows.
		next := idx + 1 + len(local)
		if next < len(s) {
			if c := s[next]; c == '>' || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/' {
				start = idx
				break
			}
		}
		i = idx + 1
	}
	closeTag := "</" + local + ">"
	end := strings.LastIndex(s, closeTag)
	if end < start {
		// Self-closing element, e.g. <configuration/>.
		if selfEnd := strings.Index(s[start:], "/>"); selfEnd >= 0 {
			return s[start : start+selfEnd+2], true
		}
		return "", false
	}
	return s[start : end+len(closeTag)], true
}

// checkRPCError reports a Junos error carried in an RPC reply. Both the CLI's
// `| display xml` output and NETCONF replies use these elements.
func checkRPCError(data []byte) error {
	for _, name := range []string{"rpc-error", "error"} {
		var e struct {
			Message  string `xml:"error-message"`
			Severity string `xml:"error-severity"`
			Path     string `xml:"error-path"`
		}
		if err := decodeFirst(data, name, &e); err != nil {
			continue
		}
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			continue
		}
		// Junos reports "warning" severity through the same element.
		if strings.EqualFold(e.Severity, "warning") {
			continue
		}
		if p := strings.TrimSpace(e.Path); p != "" {
			return fmt.Errorf("device returned %s: %s (%s)", name, msg, p)
		}
		return fmt.Errorf("device returned %s: %s", name, msg)
	}
	return nil
}
