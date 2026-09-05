// Package config loads and validates jconfig's YAML configuration and device
// inventory.
package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Transport names.
const (
	TransportSSH     = "ssh"
	TransportNETCONF = "netconf"
)

// Config is the top-level jconfig configuration.
type Config struct {
	// Listen serves metrics, liveness and readiness only. It is safe to
	// expose to a Prometheus server.
	Listen string `yaml:"listen"`
	// ManagementListen serves the control surface: device status and the
	// endpoints that trigger backups. Unset means 127.0.0.1:9640; an
	// explicit empty string disables the control surface altogether.
	ManagementListen *string   `yaml:"management_listen"`
	LogLevel         string    `yaml:"log_level"`
	LogFormat        string    `yaml:"log_format"`
	MetricsPath      string    `yaml:"metrics_path"`
	Scheduler        Scheduler `yaml:"scheduler"`
	Repo             Repo      `yaml:"repo"`
	Defaults         Device    `yaml:"defaults"`
	Devices          []Device  `yaml:"devices"`

	// Path is the file this config was loaded from.
	Path string `yaml:"-"`
}

// Scheduler controls when devices are polled.
type Scheduler struct {
	// Interval is the default backup interval for devices that do not
	// override it.
	Interval Duration `yaml:"interval"`
	// Concurrency is the maximum number of devices backed up in parallel.
	Concurrency int `yaml:"concurrency"`
	// Jitter spreads scheduled runs to avoid hammering the network.
	Jitter Duration `yaml:"jitter"`
	// RunOnStart triggers a full run when the daemon starts.
	RunOnStart *bool `yaml:"run_on_start"`
	// ScanInterval is how often the scheduler looks for due devices.
	ScanInterval Duration `yaml:"scan_interval"`
	// RetryInterval is the delay before retrying a device that failed. When
	// zero the regular interval is used.
	RetryInterval Duration `yaml:"retry_interval"`
}

// Repo describes the git repository that stores fetched configs.
type Repo struct {
	Path         string `yaml:"path"`
	Branch       string `yaml:"branch"`
	Layout       string `yaml:"layout"`
	AuthorName   string `yaml:"author_name"`
	AuthorEmail  string `yaml:"author_email"`
	CommitPrefix string `yaml:"commit_prefix"`
	// CloneInit makes a missing repository be cloned from push.url rather
	// than initialised empty, so a container starting on a blank volume
	// continues the existing history. Defaults to true when a push remote
	// is configured.
	CloneInit *bool `yaml:"clone_on_init"`
	Push      Push  `yaml:"push"`
}

// CloneOnInit reports whether a missing repository should be cloned from the
// push remote.
func (r Repo) CloneOnInit() bool { return r.CloneInit == nil || *r.CloneInit }

// Push describes an optional git remote to mirror commits to.
type Push struct {
	Enabled bool   `yaml:"enabled"`
	Remote  string `yaml:"remote"`
	URL     string `yaml:"url"`
	Branch  string `yaml:"branch"`
	KeyFile string `yaml:"key_file"`
	// Key is an inline private key, PEM or base64-encoded PEM.
	Key                   string   `yaml:"key"`
	KeyPassphrase         string   `yaml:"key_passphrase"`
	KnownHosts            string   `yaml:"known_hosts"`
	InsecureIgnoreHostKey bool     `yaml:"insecure_ignore_host_key"`
	Username              string   `yaml:"username"`
	Password              string   `yaml:"password"`
	Timeout               Duration `yaml:"timeout"`
	Force                 bool     `yaml:"force"`
}

// Device is a single box, and doubles as the defaults block.
type Device struct {
	Name string `yaml:"name" json:"name"`
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
	// Vendor selects the driver: "junos" or "routeros". Junos when unset,
	// so inventories written before RouterOS support keep working.
	Vendor    string `yaml:"vendor" json:"vendor"`
	Transport string `yaml:"transport" json:"transport"`
	Username  string `yaml:"username" json:"username"`
	Password  string `yaml:"password" json:"-"`
	KeyFile   string `yaml:"key_file" json:"-"`
	// Key is an inline private key, either PEM or base64-encoded PEM. Use
	// it with ${VAR} expansion to keep the key in a secret store instead
	// of on disk; base64 is the safe encoding there, because a raw PEM's
	// newlines would be substituted into the YAML document.
	Key                   string   `yaml:"key" json:"-"`
	KeyPassphrase         string   `yaml:"key_passphrase" json:"-"`
	KnownHosts            string   `yaml:"known_hosts" json:"-"`
	InsecureIgnoreHostKey *bool    `yaml:"insecure_ignore_host_key" json:"-"`
	HostKeyAlgorithms     []string `yaml:"host_key_algorithms" json:"-"`
	KexAlgorithms         []string `yaml:"kex_algorithms" json:"-"`
	Ciphers               []string `yaml:"ciphers" json:"-"`
	MACs                  []string `yaml:"macs" json:"-"`
	Group                 string   `yaml:"group" json:"group,omitempty"`
	Formats               []string `yaml:"formats" json:"formats"`
	Interval              Duration `yaml:"interval" json:"interval"`
	Timeout               Duration `yaml:"timeout" json:"timeout"`
	Enabled               *bool    `yaml:"enabled" json:"enabled"`
	Header                *bool    `yaml:"header" json:"-"`
	// ShowSensitive makes a RouterOS export include secrets — PSKs, PPP
	// passwords, SNMP communities — instead of the placeholders RouterOS
	// prints by default. It is what makes the export restorable, and it
	// writes those secrets into the git repository. Ignored by other
	// vendors.
	ShowSensitive *bool             `yaml:"show_sensitive" json:"-"`
	RemoveLines   []string          `yaml:"remove_lines" json:"-"`
	Labels        map[string]string `yaml:"labels" json:"labels,omitempty"`

	removeRE []*regexp.Regexp
}

// IsEnabled reports whether the device should be backed up.
func (d *Device) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// HeaderEnabled reports whether the stored configuration is prefixed with the
// inventory and licence comment block. On unless turned off.
func (d *Device) HeaderEnabled() bool { return d.Header == nil || *d.Header }

// SensitiveShown reports whether a RouterOS export should include secrets.
// Off unless turned on: an export that carries PSKs and passwords into git is
// a deliberate choice, not a default.
func (d *Device) SensitiveShown() bool { return d.ShowSensitive != nil && *d.ShowSensitive }

// SkipHostKeyCheck reports whether host key verification is disabled.
func (d *Device) SkipHostKeyCheck() bool {
	return d.InsecureIgnoreHostKey != nil && *d.InsecureIgnoreHostKey
}

// Addr returns the host:port to dial.
func (d *Device) Addr() string { return fmt.Sprintf("%s:%d", d.Host, d.Port) }

// RemoveMatchers returns the compiled remove_lines patterns.
func (d *Device) RemoveMatchers() []*regexp.Regexp { return d.removeRE }

// ManagementAddr is the address the control surface listens on, empty when it
// is disabled.
func (c *Config) ManagementAddr() string {
	if c.ManagementListen == nil {
		return ""
	}
	return *c.ManagementListen
}

// ManagementEnabled reports whether the control surface is served at all.
func (c *Config) ManagementEnabled() bool { return c.ManagementAddr() != "" }

// Load reads, expands, merges and validates the configuration at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{Path: path}
	if err := decodeStrict(expandEnv(raw), cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg.applyBuiltinDefaults()

	// Precedence: device field > defaults block > built-in.
	for i := range cfg.Devices {
		mergeDevice(&cfg.Devices[i], &cfg.Defaults)
		cfg.Devices[i].applyBuiltinDefaults(&cfg.Scheduler)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeStrict(raw []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func (c *Config) applyBuiltinDefaults() {
	setStr(&c.Listen, ":9639")
	if c.ManagementListen == nil {
		def := "127.0.0.1:9640"
		c.ManagementListen = &def
	}
	setStr(&c.LogLevel, "info")
	setStr(&c.LogFormat, "text")
	setStr(&c.MetricsPath, "/metrics")

	setStr(&c.Repo.Branch, "main")
	setStr(&c.Repo.Layout, "flat")
	setStr(&c.Repo.AuthorName, "jconfig")
	setStr(&c.Repo.AuthorEmail, "jconfig@localhost")
	c.Repo.Path = expandUser(c.Repo.Path)

	setStr(&c.Repo.Push.Remote, "origin")
	setStr(&c.Repo.Push.Branch, c.Repo.Branch)
	c.Repo.Push.KeyFile = expandUser(c.Repo.Push.KeyFile)
	c.Repo.Push.KnownHosts = expandUser(c.Repo.Push.KnownHosts)
	if c.Repo.Push.Timeout == 0 {
		c.Repo.Push.Timeout = Duration(2 * time.Minute)
	}

	if c.Scheduler.Interval == 0 {
		c.Scheduler.Interval = Duration(time.Hour)
	}
	if c.Scheduler.Concurrency <= 0 {
		c.Scheduler.Concurrency = 8
	}
	if c.Scheduler.ScanInterval == 0 {
		c.Scheduler.ScanInterval = Duration(15 * time.Second)
	}
	if c.Scheduler.RunOnStart == nil {
		c.Scheduler.RunOnStart = boolPtr(true)
	}
}

func (d *Device) applyBuiltinDefaults(s *Scheduler) {
	setStr(&d.Vendor, VendorJunos)
	setStr(&d.Transport, TransportSSH)
	// The rest is vendor knowledge; an unknown vendor is caught by
	// validate, so leave its device untouched rather than guessing.
	if v, ok := vendors[d.Vendor]; ok {
		if d.Port == 0 {
			d.Port = v.defaultPorts[d.Transport]
		}
		if len(d.Formats) == 0 {
			d.Formats = v.defaultFormats
		}
	}
	if d.Interval == 0 {
		d.Interval = s.Interval
	}
	if d.Timeout == 0 {
		d.Timeout = Duration(2 * time.Minute)
	}
	if d.KnownHosts == "" && !d.SkipHostKeyCheck() {
		d.KnownHosts = defaultKnownHosts()
	}
	d.KeyFile = expandUser(d.KeyFile)
	d.KnownHosts = expandUser(d.KnownHosts)
}

// mergeDevice fills unset fields of dst from def.
func mergeDevice(dst, def *Device) {
	setStr(&dst.Host, def.Host)
	setStr(&dst.Vendor, def.Vendor)
	setStr(&dst.Transport, def.Transport)
	setStr(&dst.Username, def.Username)
	setStr(&dst.Password, def.Password)
	setStr(&dst.KeyFile, def.KeyFile)
	setStr(&dst.Key, def.Key)
	setStr(&dst.KeyPassphrase, def.KeyPassphrase)
	setStr(&dst.KnownHosts, def.KnownHosts)
	setStr(&dst.Group, def.Group)
	if dst.Port == 0 {
		dst.Port = def.Port
	}
	if dst.InsecureIgnoreHostKey == nil {
		dst.InsecureIgnoreHostKey = def.InsecureIgnoreHostKey
	}
	if dst.Enabled == nil {
		dst.Enabled = def.Enabled
	}
	if dst.Header == nil {
		dst.Header = def.Header
	}
	if dst.ShowSensitive == nil {
		dst.ShowSensitive = def.ShowSensitive
	}
	if len(dst.HostKeyAlgorithms) == 0 {
		dst.HostKeyAlgorithms = def.HostKeyAlgorithms
	}
	if len(dst.KexAlgorithms) == 0 {
		dst.KexAlgorithms = def.KexAlgorithms
	}
	if len(dst.Ciphers) == 0 {
		dst.Ciphers = def.Ciphers
	}
	if len(dst.MACs) == 0 {
		dst.MACs = def.MACs
	}
	if len(dst.Formats) == 0 {
		dst.Formats = def.Formats
	}
	if dst.Interval == 0 {
		dst.Interval = def.Interval
	}
	if dst.Timeout == 0 {
		dst.Timeout = def.Timeout
	}
	if len(dst.RemoveLines) == 0 {
		dst.RemoveLines = def.RemoveLines
	}
	if len(def.Labels) > 0 {
		if dst.Labels == nil {
			dst.Labels = map[string]string{}
		}
		for k, v := range def.Labels {
			if _, ok := dst.Labels[k]; !ok {
				dst.Labels[k] = v
			}
		}
	}
}

func (c *Config) validate() error {
	var errs []error

	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	// Sharing a socket would put the backup trigger back on the metrics
	// port, which is the thing the split exists to prevent.
	if c.ManagementAddr() != "" && c.ManagementAddr() == c.Listen {
		errs = append(errs, errors.New("management_listen must differ from listen"))
	}

	if c.Repo.Path == "" {
		errs = append(errs, errors.New("repo.path is required"))
	}
	if c.Repo.Layout != "flat" && c.Repo.Layout != "group" {
		errs = append(errs, fmt.Errorf("repo.layout %q: must be \"flat\" or \"group\"", c.Repo.Layout))
	}
	if c.Repo.Push.Enabled && c.Repo.Push.URL == "" {
		errs = append(errs, errors.New("repo.push.enabled is true but repo.push.url is empty"))
	}
	if c.Repo.Push.Key != "" && c.Repo.Push.KeyFile != "" {
		errs = append(errs, errors.New("repo.push: set either key or key_file, not both"))
	}
	if c.Repo.Push.Key != "" {
		if _, err := DecodeKey(c.Repo.Push.Key); err != nil {
			errs = append(errs, fmt.Errorf("repo.push.key: %w", err))
		}
	}
	if len(c.Devices) == 0 {
		errs = append(errs, errors.New("no devices configured"))
	}

	seen := make(map[string]bool, len(c.Devices))
	for i := range c.Devices {
		d := &c.Devices[i]
		who := d.Name
		if who == "" {
			who = fmt.Sprintf("devices[%d]", i)
		}
		if d.Name == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", who))
		} else if seen[d.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate device name", who))
		}
		seen[d.Name] = true

		if d.Host == "" {
			errs = append(errs, fmt.Errorf("%s: host is required", who))
		}
		if !KnownVendor(d.Vendor) {
			errs = append(errs, fmt.Errorf("%s: vendor %q: want one of %s",
				who, d.Vendor, strings.Join(Vendors(), ", ")))
		} else if !supports(Transports(d.Vendor), d.Transport) {
			errs = append(errs, fmt.Errorf("%s: transport %q: vendor %s supports %s",
				who, d.Transport, d.Vendor, strings.Join(Transports(d.Vendor), ", ")))
		}
		if d.Username == "" {
			errs = append(errs, fmt.Errorf("%s: username is required", who))
		}
		if d.Password == "" && d.KeyFile == "" && d.Key == "" {
			errs = append(errs, fmt.Errorf("%s: no credentials: set password, key_file or key", who))
		}
		if d.Key != "" && d.KeyFile != "" {
			errs = append(errs, fmt.Errorf("%s: set either key or key_file, not both", who))
		}
		if d.Key != "" {
			if _, err := DecodeKey(d.Key); err != nil {
				errs = append(errs, fmt.Errorf("%s: key: %w", who, err))
			}
		}
		if KnownVendor(d.Vendor) {
			for _, f := range d.Formats {
				if d.Extension(f) == "" {
					errs = append(errs, fmt.Errorf("%s: format %q: vendor %s renders %s",
						who, f, d.Vendor, strings.Join(Formats(d.Vendor), ", ")))
				}
			}
		}
		if d.SensitiveShown() && d.Vendor != VendorRouterOS {
			errs = append(errs, fmt.Errorf("%s: show_sensitive is only meaningful for vendor routeros", who))
		}
		if !d.SkipHostKeyCheck() {
			if d.KnownHosts == "" {
				errs = append(errs, fmt.Errorf("%s: known_hosts is empty; set it or use insecure_ignore_host_key: true", who))
			} else if _, err := os.Stat(d.KnownHosts); err != nil {
				errs = append(errs, fmt.Errorf("%s: known_hosts %s: %w (set insecure_ignore_host_key: true to skip verification)", who, d.KnownHosts, err))
			}
		}
		if d.KeyFile != "" {
			if _, err := os.Stat(d.KeyFile); err != nil {
				errs = append(errs, fmt.Errorf("%s: key_file: %w", who, err))
			}
		}
		for _, pat := range d.RemoveLines {
			re, err := regexp.Compile(pat)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: remove_lines %q: %w", who, pat, err))
				continue
			}
			d.removeRE = append(d.removeRE, re)
		}
	}
	return errors.Join(errs...)
}

// EnabledDevices returns the devices that are not disabled.
func (c *Config) EnabledDevices() []Device {
	out := make([]Device, 0, len(c.Devices))
	for _, d := range c.Devices {
		if d.IsEnabled() {
			out = append(out, d)
		}
	}
	return out
}

// DecodeKey turns an inline private key into PEM bytes. It accepts a literal
// PEM block or base64-encoded PEM, so the key can travel through a single-line
// environment variable without newlines breaking the YAML document.
func DecodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty key")
	}

	if strings.Contains(s, "-----BEGIN") {
		return []byte(ensureTrailingNewline(s)), nil
	}

	// Tolerate whitespace and newlines inside the base64 payload.
	compact := strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(s)
	decoded, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return nil, fmt.Errorf("not a PEM block and not valid base64: %w", err)
	}
	if !strings.Contains(string(decoded), "-----BEGIN") {
		return nil, errors.New("decoded value is not a PEM private key")
	}
	return []byte(ensureTrailingNewline(string(decoded))), nil
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

var envRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default}. A doubled $${VAR} escapes to a
// literal ${VAR} so passwords containing the sequence survive.
func expandEnv(in []byte) []byte {
	const escape = "\x00JCONFIG_ESCAPED\x00"
	s := strings.ReplaceAll(string(in), "$${", escape)
	s = envRE.ReplaceAllStringFunc(s, func(m string) string {
		g := envRE.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(g[1]); ok && v != "" {
			return v
		}
		return g[2]
	})
	return []byte(strings.ReplaceAll(s, escape, "${"))
}

func expandUser(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		if u, uerr := user.Current(); uerr == nil {
			home = u.HomeDir
		} else {
			return p
		}
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

func defaultKnownHosts() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

func setStr(dst *string, def string) {
	if *dst == "" {
		*dst = def
	}
}

func boolPtr(b bool) *bool { return &b }
