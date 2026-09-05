// Package rostest runs a fake RouterOS device over SSH so the routeros driver
// can be tested without hardware. It answers the exec requests jconfig issues
// and nothing else.
package rostest

import (
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

// Canned RouterOS output used by the fake device. The identifiers are made up.
const (
	// User and Pass are the credentials the fake device accepts. RouterOS
	// strips the "+ct" console flags before authenticating, and so does
	// this server.
	User = "backup"
	Pass = "s3cret"

	// Banner is the timestamp line RouterOS opens every export with. It is
	// the line that moves on every fetch.
	Banner = "# 2026-09-05 10:23:45 by RouterOS 7.14.2\n"

	// Export is the compact export, with secrets masked the way RouterOS
	// masks them when show-sensitive is not asked for.
	Export = Banner + `# software id = TEST-0000
#
# model = RB5009UG+S+
# serial number = 00000000TEST
/interface bridge
add admin-mac=00:00:5E:00:53:00 auto-mac=no comment=lan name=bridge
/interface wireguard
add listen-port=51820 mtu=1420 name=wg0 private-key="..."
/ip address
add address=192.0.2.1/24 interface=bridge network=192.0.2.0
/system identity
set name=gw1-ams
`

	// SensitiveExport is what the same device renders with show-sensitive:
	// identical but for the key that is no longer masked.
	SensitiveExport = Banner + `# software id = TEST-0000
#
# model = RB5009UG+S+
# serial number = 00000000TEST
/interface bridge
add admin-mac=00:00:5E:00:53:00 auto-mac=no comment=lan name=bridge
/interface wireguard
add listen-port=51820 mtu=1420 name=wg0 private-key="0000000000000000000000000000000000000000000="
/ip address
add address=192.0.2.1/24 interface=bridge network=192.0.2.0
/system identity
set name=gw1-ams
`

	// TerseExport is the one-line-per-item rendering.
	TerseExport = Banner + `/interface bridge add admin-mac=00:00:5E:00:53:00 auto-mac=no comment=lan name=bridge
/ip address add address=192.0.2.1/24 interface=bridge network=192.0.2.0
/system identity set name=gw1-ams
`

	// VerboseExport includes the settings left at their defaults.
	VerboseExport = Banner + `/interface bridge
add admin-mac=00:00:5E:00:53:00 ageing-time=5m arp=enabled auto-mac=no comment=lan name=bridge
/system identity
set name=gw1-ams
`

	// ResourceText mixes the stable fields the header keeps with the
	// counters that move on every read and must be filtered out.
	ResourceText = `                   uptime: 3w4d13h20m
                  version: 7.14.2 (stable)
               build-time: 2026-02-15 10:00:00
         factory-software: 7.1.5
              free-memory: 900.4MiB
             total-memory: 1024.0MiB
                      cpu: ARM64
                cpu-count: 4
            cpu-frequency: 1400MHz
                 cpu-load: 3%
           free-hdd-space: 900.0MiB
          total-hdd-space: 1024.0MiB
  write-sect-since-reboot: 12345
         write-sect-total: 678910
               bad-blocks: 0%
        architecture-name: arm64
               board-name: RB5009UG+S+
                 platform: MikroTik
`

	// RouterboardText carries upgrade-firmware, which tracks what MikroTik
	// has released rather than what the device runs.
	RouterboardText = `       routerboard: yes
             model: RB5009UG+S+
          revision: r2
     serial-number: 00000000TEST
     firmware-type: dm7000
  factory-firmware: 7.1.5
  current-firmware: 7.14.2
  upgrade-firmware: 7.15.0
`

	LicenseText = `    software-id: TEST-0000
         nlevel: 4
       features:
`

	IdentityText = "  name: gw1-ams\n"
)

// Server is an SSH server that answers the handful of RouterOS commands
// jconfig issues.
type Server struct {
	ln         net.Listener
	signer     ssh.Signer
	wg         sync.WaitGroup
	closing    chan struct{}
	knownHosts string

	mu sync.Mutex
	// failCommand makes any command containing it answer with a RouterOS
	// error banner on a zero exit status.
	failCommand string
	// exportOverride replaces the compact export when non-empty.
	exportOverride string
	// logins records the user names the server was asked to authenticate.
	logins []string
}

// New starts a fake RouterOS device on a random loopback port. The caller owns
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

// Start launches a fake RouterOS device that is shut down when the test ends.
func Start(tb testing.TB) *Server {
	tb.Helper()

	f, err := New(tb.TempDir())
	if err != nil {
		tb.Fatalf("start fake routeros: %v", err)
	}
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

// SetFailCommand makes any command containing cmd answer with a RouterOS
// error instead of output.
func (f *Server) SetFailCommand(cmd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCommand = cmd
}

// SetExport replaces the compact export the device serves.
func (f *Server) SetExport(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exportOverride = s
}

// Logins returns the user names the server has been asked to authenticate, so
// a test can check the console flags were offered.
func (f *Server) Logins() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logins...)
}

func (f *Server) export() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exportOverride != "" {
		return f.exportOverride
	}
	return Export
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

// account strips the "+flags" suffix RouterOS allows on a login name.
func account(user string) string {
	if i := strings.Index(user, "+"); i >= 0 {
		return user[:i]
	}
	return user
}

func (f *Server) recordLogin(user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins = append(f.logins, user)
}

func (f *Server) serve(nConn net.Conn) {
	defer func() { _ = nConn.Close() }()

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			f.recordLogin(c.User())
			if account(c.User()) == User && string(pass) == Pass {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
		// Any key is accepted: these tests are about jconfig offering a
		// key, not about authorising a particular one.
		PublicKeyCallback: func(c ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			f.recordLogin(c.User())
			if account(c.User()) == User {
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
		// RouterOS has no subsystems jconfig uses; only exec is answered.
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		_ = ssh.Unmarshal(req.Payload, &payload)
		_ = req.Reply(true, nil)
		out, code := f.runCommand(payload.Command)
		_, _ = io.WriteString(ch, out)
		sendExit(ch, code)
		_ = ch.Close()
		return
	}
}

func sendExit(ch ssh.Channel, code uint32) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}

// runCommand maps a RouterOS command to canned output. RouterOS reports a bad
// command on stdout with a zero exit status, which is what makes the driver's
// output check necessary, so the fake device does the same.
func (f *Server) runCommand(cmd string) (string, uint32) {
	f.mu.Lock()
	fail := f.failCommand
	f.mu.Unlock()

	cmd = strings.TrimSpace(cmd)
	if fail != "" && strings.Contains(cmd, fail) {
		return "bad command name " + cmd + " (line 1 column 1)\n", 0
	}

	sensitive := strings.Contains(cmd, "show-sensitive")
	base := strings.TrimSpace(strings.ReplaceAll(cmd, "show-sensitive", ""))

	switch base {
	case "/export":
		if sensitive {
			return SensitiveExport, 0
		}
		return f.export(), 0
	case "/export verbose":
		return VerboseExport, 0
	case "/export terse":
		return TerseExport, 0
	case "/system resource print":
		return ResourceText, 0
	case "/system routerboard print":
		return RouterboardText, 0
	case "/system license print":
		return LicenseText, 0
	case "/system identity print":
		return IdentityText, 0
	default:
		return "bad command name " + cmd + " (line 1 column 1)\n", 0
	}
}
