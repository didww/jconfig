// Package sshx is the SSH plumbing shared by every vendor driver: dialling,
// host key verification, credentials and running a command. It knows nothing
// about what the commands mean.
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/device"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
)

// Conn is an SSH connection plus the raw socket underneath it, which is kept
// so deadlines and cancellation can be enforced on every read.
type Conn struct {
	Client *ssh.Client
	raw    net.Conn
}

func (c *Conn) Close() {
	if c.Client != nil {
		_ = c.Client.Close()
	}
	if c.raw != nil {
		_ = c.raw.Close()
	}
}

// SetDeadline applies ctx's deadline, if any, to the underlying socket.
func (c *Conn) SetDeadline(ctx context.Context) {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.raw.SetDeadline(dl)
	}
}

// Watch closes the connection if ctx is cancelled, unblocking any in-flight
// read. The returned func stops the watcher.
func (c *Conn) Watch(ctx context.Context) func() {
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

// Dial opens an SSH connection to the device. The username is taken from
// login, not from d, so a driver can pass the vendor's console flags along
// with it.
func Dial(ctx context.Context, d *config.Device, login string) (*Conn, error) {
	auth, err := authMethods(d)
	if err != nil {
		return nil, device.StageErr(device.StageAuth, err)
	}

	cc := &ssh.ClientConfig{
		User:              login,
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
			return nil, device.StageErr(device.StageConnect, fmt.Errorf("known_hosts %s: %w", d.KnownHosts, err))
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
		return nil, device.StageErr(device.StageConnect, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}

	sc, chans, reqs, err := ssh.NewClientConn(raw, d.Addr(), cc)
	if err != nil {
		_ = raw.Close()
		return nil, classifyHandshakeError(d, err)
	}
	return &Conn{Client: ssh.NewClient(sc, chans, reqs), raw: raw}, nil
}

func classifyHandshakeError(d *config.Device, err error) error {
	switch {
	case knownhosts.IsHostUnknown(err):
		return device.StageErr(device.StageConnect, fmt.Errorf("host key for %s not in %s: %w", d.Addr(), d.KnownHosts, err))
	case knownhosts.IsHostKeyChanged(err):
		return device.StageErr(device.StageConnect, fmt.Errorf("host key for %s CHANGED, refusing to connect: %w", d.Addr(), err))
	case strings.Contains(err.Error(), "unable to authenticate"),
		strings.Contains(err.Error(), "no supported methods remain"):
		return device.StageErr(device.StageAuth, err)
	default:
		return device.StageErr(device.StageConnect, err)
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
		// Devices often present the password prompt as keyboard-interactive.
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

// Run executes a single command in its own session.
func (c *Conn) Run(ctx context.Context, cmd string) ([]byte, error) {
	c.SetDeadline(ctx)
	stop := c.Watch(ctx)
	defer stop()

	sess, err := c.Client.NewSession()
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
