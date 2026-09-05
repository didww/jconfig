package junos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/device"
	"github.com/didww/jconfig/internal/sshx"
	"golang.org/x/crypto/ssh"
)

// NETCONF end-of-message delimiter for base:1.0 framing (RFC 6242 §4.3).
var netconfDelim = []byte("]]>]]>")

// maxMessageSize caps a single NETCONF reply so a misbehaving device cannot
// exhaust memory. Even a very large Junos config is far below this.
const maxMessageSize = 256 << 20

const clientHello = `<?xml version="1.0" encoding="UTF-8"?>
<hello xmlns="urn:ietf:params:xml:ns:netconf:base:1.0">
  <capabilities>
    <capability>urn:ietf:params:netconf:base:1.0</capability>
  </capabilities>
</hello>
]]>]]>
`

type netconfSession struct {
	conn  *sshx.Conn
	sess  *ssh.Session
	stdin io.WriteCloser
	out   io.Reader
	msgID int

	// buf holds bytes read from the device that have not been consumed yet.
	// A single Read can span the end of one message and the start of the
	// next, so the remainder has to survive across calls to read.
	buf bytes.Buffer
}

func openNETCONF(ctx context.Context, conn *sshx.Conn) (*netconfSession, error) {
	sess, err := conn.Client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := sess.RequestSubsystem("netconf"); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("request netconf subsystem: %w (is \"set system services netconf ssh\" configured?)", err)
	}

	s := &netconfSession{conn: conn, sess: sess, stdin: stdin, out: stdout}

	// Server hello first, then ours.
	if _, err := s.read(ctx); err != nil {
		return nil, fmt.Errorf("read server hello: %w", err)
	}
	if _, err := io.WriteString(stdin, clientHello); err != nil {
		return nil, fmt.Errorf("send client hello: %w", err)
	}
	return s, nil
}

func (s *netconfSession) Close() {
	if s.stdin != nil {
		_, _ = io.WriteString(s.stdin, "<rpc message-id=\"end\"><close-session/></rpc>\n]]>]]>\n")
		_ = s.stdin.Close()
	}
	if s.sess != nil {
		_ = s.sess.Close()
	}
}

// read returns the next NETCONF message, stripped of its delimiter. Anything
// read past the delimiter stays buffered for the following call.
func (s *netconfSession) read(ctx context.Context) ([]byte, error) {
	stop := s.conn.Watch(ctx)
	defer stop()

	searched := 0
	tmp := make([]byte, 32*1024)
	for {
		if idx := bytes.Index(s.buf.Bytes()[searched:], netconfDelim); idx >= 0 {
			end := searched + idx
			msg := make([]byte, end)
			copy(msg, s.buf.Bytes()[:end])
			s.buf.Next(end + len(netconfDelim))
			return msg, nil
		}
		// Only the last few bytes can hold a partial delimiter.
		if adv := s.buf.Len() - len(netconfDelim) + 1; adv > searched {
			searched = adv
		}

		n, err := s.out.Read(tmp)
		if n > 0 {
			s.buf.Write(tmp[:n])
			if s.buf.Len() > maxMessageSize {
				return nil, fmt.Errorf("reply exceeds %d bytes", maxMessageSize)
			}
			continue
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if errors.Is(err, io.EOF) {
				if s.buf.Len() == 0 {
					return nil, errors.New("connection closed by device")
				}
				return nil, fmt.Errorf("truncated reply (%d bytes, no delimiter)", s.buf.Len())
			}
			return nil, err
		}
	}
}

// rpc sends one RPC and returns the reply body.
func (s *netconfSession) rpc(ctx context.Context, body string) ([]byte, error) {
	s.conn.SetDeadline(ctx)
	s.msgID++
	// No xmlns on <rpc>: this is what Juniper's own examples send, and Junos
	// resolves the child RPC against its native namespace.
	msg := fmt.Sprintf("<rpc message-id=\"%d\">%s</rpc>\n%s\n", s.msgID, body, netconfDelim)
	if _, err := io.WriteString(s.stdin, msg); err != nil {
		return nil, fmt.Errorf("send rpc: %w", err)
	}
	reply, err := s.read(ctx)
	if err != nil {
		return nil, fmt.Errorf("read reply: %w", err)
	}
	if err := checkRPCError(reply); err != nil {
		return nil, err
	}
	return reply, nil
}

func fetchNETCONF(ctx context.Context, d *config.Device) (*device.Result, error) {
	conn, err := sshx.Dial(ctx, d, d.Username)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	sess, err := openNETCONF(ctx, conn)
	if err != nil {
		return nil, device.StageErr(device.StageConnect, err)
	}
	defer sess.Close()

	res := &device.Result{Configs: make(map[string]string, len(d.Formats))}

	for _, format := range d.Formats {
		reply, err := sess.rpc(ctx, netconfConfigRPC(format))
		if err != nil {
			return nil, device.StageErr(device.StageFetch, fmt.Errorf("get-configuration format=%s: %w", format, err))
		}
		content, err := parseNETCONFConfig(format, reply)
		if err != nil {
			return nil, device.StageErr(device.StageParse, fmt.Errorf("get-configuration format=%s: %w", format, err))
		}
		if strings.TrimSpace(content) == "" {
			return nil, device.StageErr(device.StageParse, fmt.Errorf("get-configuration format=%s: device returned an empty configuration", format))
		}
		res.Configs[format] = content
	}

	if reply, err := sess.rpc(ctx, "<get-software-information/>"); err == nil {
		applyVersion(res, reply)
	}
	if reply, err := sess.rpc(ctx, "<get-commit-information/>"); err == nil {
		applyCommit(res, reply)
	}
	for _, cmd := range header {
		out, err := sess.commandText(ctx, cmd)
		if err != nil {
			out = ""
		}
		res.HeaderBlocks = append(res.HeaderBlocks, out)
	}
	return res, nil
}

// commandText runs an operational command and returns its CLI rendering. Junos
// answers <command format="text"> with the same text the CLI prints, which is
// what keeps the header identical across the two transports; the dedicated
// RPCs would return XML that each release structures differently.
func (s *netconfSession) commandText(ctx context.Context, cmd string) (string, error) {
	reply, err := s.rpc(ctx, fmt.Sprintf("<command format=\"text\">%s</command>", cmd))
	if err != nil {
		return "", err
	}
	return elementText(reply, "output")
}

func netconfConfigRPC(format string) string {
	switch format {
	case config.FormatSet:
		return `<get-configuration database="committed" format="set"/>`
	case config.FormatXML:
		return `<get-configuration database="committed"/>`
	default:
		return `<get-configuration database="committed" format="text"/>`
	}
}

func parseNETCONFConfig(format string, reply []byte) (string, error) {
	switch format {
	case config.FormatSet:
		return elementText(reply, "configuration-set")
	case config.FormatXML:
		raw, ok := rawElement(reply, "configuration")
		if !ok {
			return "", fmt.Errorf("no <configuration> element in reply: %s", snippet(reply))
		}
		return raw, nil
	default:
		return elementText(reply, "configuration-text")
	}
}
