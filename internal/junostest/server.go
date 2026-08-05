package junostest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
)

// Canned Junos output used by the fake device.
const (
	// User and Pass are the credentials the fake device accepts.
	User = "backup"
	Pass = "s3cret"

	// TextConfig is the curly-brace configuration the device serves.
	TextConfig = `## Last commit: 2026-07-20 10:00:00 UTC by admin
version 21.4R3-S4.9;
system {
    host-name mx1-ams;
    services {
        netconf {
            ssh;
        }
    }
}
`

	// SetConfig is the "display set" rendering of TextConfig.
	SetConfig = `set version 21.4R3-S4.9
set system host-name mx1-ams
set system services netconf ssh
`

	xmlConfig = `<configuration junos:changed-seconds="1784894400">
    <version>21.4R3-S4.9</version>
    <system>
        <host-name>mx1-ams</host-name>
    </system>
</configuration>`

	versionXML = `<?xml version="1.0" encoding="us-ascii"?>
<rpc-reply xmlns:junos="http://xml.juniper.net/junos/21.4R0/junos">
<software-information>
<host-name>mx1-ams</host-name>
<product-model>mx480</product-model>
<product-name>mx480</product-name>
<junos-version>21.4R3-S4.9</junos-version>
<package-information>
<name>junos-version</name>
<comment>JUNOS Software Release [21.4R3-S4.9]</comment>
</package-information>
</software-information>
</rpc-reply>`

	// InventoryText and LicenseText are the CLI output of
	// `show chassis hardware` and `show system license`. Both label their own
	// output, exactly as a device does.
	InventoryText = `Hardware inventory:
Item             Version  Part number  Serial number     Description
Chassis                                JN123456AB        MX480
Routing Engine   REV 06   750-095568   CAAB1234          RE-S-1800x4
FPC 0                     BUILTIN      BUILTIN           FPC
  PIC 0                                                  4x10GE SFP+
`

	LicenseText = `License usage:
                                 Licensed     Licensed    Licensed
                                  Feature      Feature     Feature
  Feature name                       used    installed      needed    Expiry
  scale-subscriber                      0            8           0    permanent

Licenses installed: none
`

	commitXML = `<?xml version="1.0" encoding="us-ascii"?>
<rpc-reply xmlns:junos="http://xml.juniper.net/junos/21.4R0/junos">
<commit-information>
<commit-history>
<sequence-number>0</sequence-number>
<user>admin</user>
<client>cli</client>
<date-time junos:seconds="1784894400">2026-07-20 10:00:00 UTC</date-time>
</commit-history>
<commit-history>
<sequence-number>1</sequence-number>
<user>noc</user>
<date-time junos:seconds="1784808000">2026-07-19 10:00:00 UTC</date-time>
</commit-history>
</commit-information>
</rpc-reply>`
)

// Server is an SSH server that answers the handful of CLI commands and
// NETCONF RPCs jconfig issues.
// Server is an SSH server that answers the handful of CLI commands and
// NETCONF RPCs jconfig issues, so transports can be tested without a device.
type Server struct {
	tb         testing.TB
	ln         net.Listener
	signer     ssh.Signer
	wg         sync.WaitGroup
	closing    chan struct{}
	knownHosts string

	mu sync.Mutex
	// failCommand makes the named CLI command return a Junos error.
	failCommand string
	// noNetconf refuses the netconf subsystem request.
	noNetconf bool
	// textOverride replaces the text configuration when non-empty.
	textOverride string
}

// New starts a fake Junos device on a random loopback port. The caller owns
// the returned server and must Close it; dir holds the generated known_hosts
// file.
func New(dir string) (*Server, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("signer: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	f := &Server{ln: ln, signer: signer, closing: make(chan struct{})}

	// Write a known_hosts file naming this listener.
	line := knownhosts.Line([]string{ln.Addr().String()}, signer.PublicKey())
	f.knownHosts = filepath.Join(dir, fmt.Sprintf("known_hosts-%s", portOf(ln.Addr().String())))
	if err := os.WriteFile(f.knownHosts, []byte(line+"\n"), 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("write known_hosts: %w", err)
	}

	f.wg.Add(1)
	go f.acceptLoop()
	return f, nil
}

// Start launches a fake Junos device that is shut down when the test ends.
func Start(tb testing.TB) *Server {
	tb.Helper()

	f, err := New(tb.TempDir())
	if err != nil {
		tb.Fatalf("start fake junos: %v", err)
	}
	f.tb = tb
	tb.Cleanup(f.Close)
	return f
}

// Addr is the host:port the fake device listens on.
func (f *Server) Addr() string { return f.ln.Addr().String() }

// Host is the listening address without the port.
func (f *Server) Host() string { h, _, _ := net.SplitHostPort(f.ln.Addr().String()); return h }

// Port is the listening port.
func (f *Server) Port() int { var p int; fmt.Sscanf(portOf(f.ln.Addr().String()), "%d", &p); return p }

// KnownHosts is a known_hosts file naming this server.
func (f *Server) KnownHosts() string { return f.knownHosts }

func portOf(addr string) string { _, p, _ := net.SplitHostPort(addr); return p }

// Close stops the server.
func (f *Server) Close() {
	select {
	case <-f.closing:
		return
	default:
	}
	close(f.closing)
	_ = f.ln.Close()
	f.wg.Wait()
}

// SetFailCommand makes any CLI command containing cmd return a Junos error.
func (f *Server) SetFailCommand(cmd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCommand = cmd
}

// SetNoNetconf makes the device refuse the netconf subsystem.
func (f *Server) SetNoNetconf(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noNetconf = v
}

// SetText replaces the text configuration the device serves.
func (f *Server) SetText(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.textOverride = s
}

func (f *Server) text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.textOverride != "" {
		return f.textOverride
	}
	return TextConfig
}

func (f *Server) acceptLoop() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.serve(conn)
		}()
	}
}

func (f *Server) serve(nConn net.Conn) {
	defer func() { _ = nConn.Close() }()

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == User && string(pass) == Pass {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
		// Any key is accepted: these tests are about jconfig loading and
		// offering a key, not about authorising a particular one.
		PublicKeyCallback: func(c ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == User {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	cfg.AddHostKey(f.signer)

	sConn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sConn.Close() }()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			return
		}
		go f.handleSession(ch, chReqs)
	}
}

func (f *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			_ = ssh.Unmarshal(req.Payload, &payload)
			_ = req.Reply(true, nil)
			out, code := f.runCommand(payload.Command)
			_, _ = io.WriteString(ch, out)
			sendExit(ch, code)
			_ = ch.Close()
			return

		case "subsystem":
			var payload struct{ Name string }
			_ = ssh.Unmarshal(req.Payload, &payload)
			f.mu.Lock()
			refuse := f.noNetconf
			f.mu.Unlock()
			if payload.Name != "netconf" || refuse {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			f.netconfLoop(ch)
			sendExit(ch, 0)
			_ = ch.Close()
			return

		default:
			_ = req.Reply(false, nil)
		}
	}
}

func sendExit(ch ssh.Channel, code uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}

// runCommand maps a Junos CLI command to canned output.
func (f *Server) runCommand(cmd string) (string, uint32) {
	f.mu.Lock()
	fail := f.failCommand
	f.mu.Unlock()

	cmd = strings.TrimSpace(cmd)
	if fail != "" && strings.Contains(cmd, fail) {
		// Junos reports errors on stdout with a zero exit status.
		return "error: syntax error, expecting <command>\n", 0
	}

	switch {
	case strings.HasPrefix(cmd, "show configuration | display set"):
		return SetConfig, 0
	case strings.HasPrefix(cmd, "show configuration | display xml"):
		return fmt.Sprintf(`<?xml version="1.0" encoding="us-ascii"?>
<rpc-reply xmlns:junos="http://xml.juniper.net/junos/21.4R0/junos">
%s
</rpc-reply>`, xmlConfig), 0
	case strings.HasPrefix(cmd, "show configuration"):
		return f.text(), 0
	case strings.HasPrefix(cmd, "show version"):
		return versionXML, 0
	case strings.HasPrefix(cmd, "show system commit"):
		return commitXML, 0
	case strings.HasPrefix(cmd, "show chassis hardware"):
		return InventoryText, 0
	case strings.HasPrefix(cmd, "show system license"):
		return LicenseText, 0
	default:
		return "error: unknown command: " + cmd + "\n", 0
	}
}

// netconfLoop speaks base:1.0 framed NETCONF.
func (f *Server) netconfLoop(ch ssh.Channel) {
	hello := `<?xml version="1.0" encoding="UTF-8"?>
<hello xmlns="urn:ietf:params:xml:ns:netconf:base:1.0">
<capabilities>
<capability>urn:ietf:params:netconf:base:1.0</capability>
<capability>http://xml.juniper.net/netconf/junos/1.0</capability>
</capabilities>
<session-id>42</session-id>
</hello>
]]>]]>
`
	if _, err := io.WriteString(ch, hello); err != nil {
		return
	}

	fr := &framed{r: ch}
	for {
		msg, err := fr.next()
		if err != nil {
			return
		}
		switch {
		case strings.Contains(msg, "<hello"):
			continue

		case strings.Contains(msg, "<close-session"):
			_, _ = io.WriteString(ch, wrapReply("<ok/>"))
			return

		case strings.Contains(msg, "<get-configuration"):
			var body string
			switch {
			case strings.Contains(msg, `format="set"`):
				body = "<configuration-set>" + escape(SetConfig) + "</configuration-set>"
			case strings.Contains(msg, `format="text"`):
				body = "<configuration-text>" + escape(f.text()) + "</configuration-text>"
			default:
				body = xmlConfig
			}
			_, _ = io.WriteString(ch, wrapReply(body))

		case strings.Contains(msg, "<get-software-information"):
			_, _ = io.WriteString(ch, versionXML+"\n]]>]]>\n")

		case strings.Contains(msg, "<get-commit-information"):
			_, _ = io.WriteString(ch, commitXML+"\n]]>]]>\n")

		case strings.Contains(msg, "<command"):
			out := ""
			switch {
			case strings.Contains(msg, "show chassis hardware"):
				out = InventoryText
			case strings.Contains(msg, "show system license"):
				out = LicenseText
			}
			_, _ = io.WriteString(ch, wrapReply("<output>"+escape(out)+"</output>"))

		default:
			_, _ = io.WriteString(ch, wrapReply(
				`<rpc-error><error-severity>error</error-severity>`+
					`<error-message>unsupported rpc</error-message></rpc-error>`))
		}
	}
}

func wrapReply(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<rpc-reply xmlns:junos="http://xml.juniper.net/junos/21.4R0/junos">` +
		body + "</rpc-reply>\n]]>]]>\n"
}

func escape(s string) string {
	var b bytes.Buffer
	_ = xmlEscape(&b, s)
	return b.String()
}

func xmlEscape(w io.Writer, s string) error {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	_, err := io.WriteString(w, r.Replace(s))
	return err
}

var delim = []byte("]]>]]>")

// framed reads base:1.0 framed NETCONF messages. Bytes that arrive after a
// delimiter belong to the next message and must be kept, otherwise a client
// that writes its hello and first RPC back to back loses the RPC.
type framed struct {
	r   io.Reader
	buf bytes.Buffer
}

func (f *framed) next() (string, error) {
	tmp := make([]byte, 4096)
	for {
		if idx := bytes.Index(f.buf.Bytes(), delim); idx >= 0 {
			msg := string(f.buf.Bytes()[:idx])
			f.buf.Next(idx + len(delim))
			return msg, nil
		}
		n, err := f.r.Read(tmp)
		if n > 0 {
			f.buf.Write(tmp[:n])
			continue
		}
		if err != nil {
			return "", err
		}
	}
}
