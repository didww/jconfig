package junos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/didww/jconfig/internal/config"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
)

// sshConn is an SSH connection plus the raw socket underneath it, which is
// kept so deadlines and cancellation can be enforced on every read.
type sshConn struct {
	client *ssh.Client
	raw    net.Conn
}

func (c *sshConn) Close() {
	if c.client != nil {
		_ = c.client.Close()
	}
	if c.raw != nil {
		_ = c.raw.Close()
	}
}

// watch closes the connection if ctx is cancelled, unblocking any in-flight
// read. The returned func stops the watcher.
func (c *sshConn) watch(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = c.raw.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func dial(ctx context.Context, d *config.Device) (*sshConn, error) {
	auth, err := authMethods(d)
	if err != nil {
		return nil, stageErr(StageAuth, err)
	}

	cc := &ssh.ClientConfig{
		User:              d.Username,
		Auth:              auth,
		HostKeyAlgorithms: d.HostKeyAlgorithms,
		Timeout:           d.Timeout.Duration(),
	}
	cc.KeyExchanges = d.KexAlgorithms
	cc.Ciphers = d.Ciphers
	cc.MACs = d.MACs

	if d.SkipHostKeyCheck() {
		cc.HostKeyCallback = ssh.InsecureIgnoreHostKey() //nolint:gosec // opt-in
	} else {
		db, err := knownhosts.NewDB(d.KnownHosts)
		if err != nil {
			return nil, stageErr(StageConnect, fmt.Errorf("known_hosts %s: %w", d.KnownHosts, err))
		}
		cc.HostKeyCallback = db.HostKeyCallback()
		if len(cc.HostKeyAlgorithms) == 0 {
			// Offer exactly what we have on file, so a host with several
			// key types does not fail verification on the wrong one.
			cc.HostKeyAlgorithms = db.HostKeyAlgorithms(d.Addr())
		}
	}

	dialer := &net.Dialer{}
	raw, err := dialer.DialContext(ctx, "tcp", d.Addr())
	if err != nil {
		return nil, stageErr(StageConnect, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}

	sc, chans, reqs, err := ssh.NewClientConn(raw, d.Addr(), cc)
	if err != nil {
		_ = raw.Close()
		return nil, classifyHandshakeError(d, err)
	}
	return &sshConn{client: ssh.NewClient(sc, chans, reqs), raw: raw}, nil
}

func classifyHandshakeError(d *config.Device, err error) error {
	switch {
	case knownhosts.IsHostUnknown(err):
		return stageErr(StageConnect, fmt.Errorf("host key for %s not in %s: %w", d.Addr(), d.KnownHosts, err))
	case knownhosts.IsHostKeyChanged(err):
		return stageErr(StageConnect, fmt.Errorf("host key for %s CHANGED, refusing to connect: %w", d.Addr(), err))
	case strings.Contains(err.Error(), "unable to authenticate"),
		strings.Contains(err.Error(), "no supported methods remain"):
		return stageErr(StageAuth, err)
	default:
		return stageErr(StageConnect, err)
	}
}

func authMethods(d *config.Device) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if d.Key != "" || d.KeyFile != "" {
		pem, source, err := privateKey(d)
		if err != nil {
			return nil, err
		}
		var signer ssh.Signer
		if d.KeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(d.KeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(pem)
		}
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", source, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if d.Password != "" {
		methods = append(methods, ssh.Password(d.Password))
		// Junos often presents the password prompt as keyboard-interactive.
		methods = append(methods, ssh.KeyboardInteractive(
			func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = d.Password
				}
				return answers, nil
			}))
	}

	if len(methods) == 0 {
		return nil, errors.New("no usable credentials")
	}
	return methods, nil
}

// privateKey returns the device's private key, from the inline value or the
// key file, along with a label naming which one for error messages.
func privateKey(d *config.Device) (pem []byte, source string, err error) {
	if d.Key != "" {
		pem, err = config.DecodeKey(d.Key)
		if err != nil {
			return nil, "key", fmt.Errorf("key: %w", err)
		}
		return pem, "key", nil
	}
	pem, err = os.ReadFile(d.KeyFile)
	if err != nil {
		return nil, "key_file", fmt.Errorf("read key_file: %w", err)
	}
	return pem, "key_file " + d.KeyFile, nil
}

// run executes a single CLI command in its own session.
func (c *sshConn) run(ctx context.Context, cmd string) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.raw.SetDeadline(dl)
	}
	stop := c.watch(ctx)
	defer stop()

	sess, err := c.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	if err := sess.Run(cmd); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%q: %w: %s", cmd, err, msg)
		}
		return nil, fmt.Errorf("%q: %w", cmd, err)
	}
	return stdout.Bytes(), nil
}

func fetchSSH(ctx context.Context, d *config.Device) (*Result, error) {
	conn, err := dial(ctx, d)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	res := &Result{Configs: make(map[string]string, len(d.Formats))}

	for _, format := range d.Formats {
		cmd := cliConfigCommand(format)
		out, err := conn.run(ctx, cmd)
		if err != nil {
			return nil, stageErr(StageFetch, err)
		}
		content, err := parseCLIConfig(format, out)
		if err != nil {
			return nil, stageErr(StageParse, fmt.Errorf("%q: %w", cmd, err))
		}
		if strings.TrimSpace(content) == "" {
			return nil, stageErr(StageParse, fmt.Errorf("%q: device returned an empty configuration", cmd))
		}
		res.Configs[format] = content
	}

	// Metadata is best effort: a device that will not report its version is
	// still worth backing up.
	if out, err := conn.run(ctx, "show version | display xml | no-more"); err == nil {
		applyVersion(res, out)
	}
	if out, err := conn.run(ctx, "show system commit | display xml | no-more"); err == nil {
		applyCommit(res, out)
	}
	if out, err := conn.run(ctx, "show version | no-more"); err == nil {
		res.Version = cliText(out)
	}
	if out, err := conn.run(ctx, "show chassis hardware | no-more"); err == nil {
		res.Inventory = cliText(out)
	}
	if out, err := conn.run(ctx, "show system license | no-more"); err == nil {
		res.Licenses = cliText(out)
	}
	if out, err := conn.run(ctx, "show virtual-chassis | no-more"); err == nil {
		res.VirtualChassis = cliText(out)
	}
	return res, nil
}

// cliText returns operational output, or "" when the device answered with an
// error banner instead. A class that may read the configuration but not the
// chassis prints one on stdout with a zero exit status, and it must not end up
// commented into the repository.
func cliText(out []byte) string {
	if err := cliTextError(out); err != nil {
		return ""
	}
	return string(out)
}

func cliConfigCommand(format string) string {
	switch format {
	case config.FormatSet:
		return "show configuration | display set | no-more"
	case config.FormatXML:
		return "show configuration | display xml | no-more"
	default:
		return "show configuration | no-more"
	}
}

func parseCLIConfig(format string, out []byte) (string, error) {
	if format == config.FormatXML {
		if err := checkRPCError(out); err != nil {
			return "", err
		}
		raw, ok := rawElement(out, "configuration")
		if !ok {
			return "", fmt.Errorf("no <configuration> element in reply: %s", snippet(out))
		}
		return raw, nil
	}
	if err := cliTextError(out); err != nil {
		return "", err
	}
	return string(out), nil
}

// cliTextError catches the plain-text errors the CLI prints on stdout with a
// zero exit status, such as an account that lands in the shell instead of the
// CLI.
func cliTextError(out []byte) error {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		low := strings.ToLower(line)
		switch {
		case strings.HasPrefix(low, "error:"):
			return errors.New(line)
		case strings.Contains(low, "unknown command"),
			strings.Contains(low, "syntax error"),
			strings.Contains(low, "permission denied"):
			return fmt.Errorf("%s (is the login class allowed to run \"show configuration\"?)", line)
		}
		// Only the first few lines can carry an error banner; a valid
		// config starts immediately.
		return nil
	}
	return errors.New("device returned no output")
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
