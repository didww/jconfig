package junos

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/didww/jconfig/internal/config"
	"github.com/didww/jconfig/internal/device"
	"github.com/didww/jconfig/internal/sshx"
)

func fetchSSH(ctx context.Context, d *config.Device) (*device.Result, error) {
	conn, err := sshx.Dial(ctx, d, d.Username)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	res := &device.Result{Configs: make(map[string]string, len(d.Formats))}

	for _, format := range d.Formats {
		cmd := cliConfigCommand(format)
		out, err := conn.Run(ctx, cmd)
		if err != nil {
			return nil, device.StageErr(device.StageFetch, err)
		}
		content, err := parseCLIConfig(format, out)
		if err != nil {
			return nil, device.StageErr(device.StageParse, fmt.Errorf("%q: %w", cmd, err))
		}
		if strings.TrimSpace(content) == "" {
			return nil, device.StageErr(device.StageParse, fmt.Errorf("%q: device returned an empty configuration", cmd))
		}
		res.Configs[format] = content
	}

	// Metadata is best effort: a device that will not report its version is
	// still worth backing up.
	if out, err := conn.Run(ctx, "show version | display xml | no-more"); err == nil {
		applyVersion(res, out)
	}
	if out, err := conn.Run(ctx, "show system commit | display xml | no-more"); err == nil {
		applyCommit(res, out)
	}
	for _, cmd := range header {
		out, err := conn.Run(ctx, cmd+" | no-more")
		if err != nil {
			res.HeaderBlocks = append(res.HeaderBlocks, "")
			continue
		}
		res.HeaderBlocks = append(res.HeaderBlocks, cliText(out))
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
