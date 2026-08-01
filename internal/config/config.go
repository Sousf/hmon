// Package config loads and validates hmon's configuration file.
//
// The guiding constraint is that ~/.ssh/config already holds every connection
// detail — aliases, identity files, ports, ProxyJump — so a host here is
// usually just a name. Adding a machine to the fleet is meant to be one line.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Sousf/hmon/internal/model"
)

// Defaults applied when the file omits a setting.
const (
	DefaultInterval = 2 * time.Second
	DefaultTimeout  = 5 * time.Second
	// DefaultCommandTimeout is generous compared to the poll timeout on
	// purpose: a poll must fail fast so one slow host does not stall the table,
	// but an ad-hoc command is something the operator is deliberately waiting
	// on and may legitimately take much longer.
	DefaultCommandTimeout = 60 * time.Second
)

// Limits is a warn/critical pair. Thresholds drive nothing but the colour a
// value is drawn in — there are deliberately no notifications behind them.
type Limits struct {
	Warn float64 `yaml:"warn"`
	Crit float64 `yaml:"crit"`
}

// Level classifies a reading against its limits.
type Level int

const (
	LevelOK Level = iota
	LevelWarn
	LevelCrit
)

// Classify reports which band a value falls into.
func (l Limits) Classify(v float64) Level {
	switch {
	case l.Crit > 0 && v >= l.Crit:
		return LevelCrit
	case l.Warn > 0 && v >= l.Warn:
		return LevelWarn
	default:
		return LevelOK
	}
}

// Thresholds groups the limits for every coloured metric.
type Thresholds struct {
	CPU  Limits `yaml:"cpu"`
	Mem  Limits `yaml:"mem"`
	Disk Limits `yaml:"disk"`
	Temp Limits `yaml:"temp"`
}

// Host is one machine in the fleet. It unmarshals from either a bare string or
// a mapping — see UnmarshalYAML.
type Host struct {
	Addr        string `yaml:"host"` // what ssh connects to
	DisplayName string `yaml:"name"` // what the table shows
	// Each of these overrides the global list of the same name for this host.
	// Empty inherits the global setting.
	Filesystems []string `yaml:"filesystems"`
	Services    []string `yaml:"services"`
	Containers  []string `yaml:"containers"`
}

// Name is the label shown in the UI, defaulting to the ssh target.
func (h Host) Name() string {
	if h.DisplayName != "" {
		return h.DisplayName
	}
	return h.Addr
}

// UnmarshalYAML accepts both spellings of a host entry:
//
//	hosts:
//	  - nas                      # scalar: sugar for {host: nas}
//	  - host: media-01.lan       # mapping: when extra settings are needed
//	    name: media
//
// One type with two spellings means nothing downstream has to branch.
func (h *Host) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		h.Addr = s
		return nil
	}

	// The decoder's KnownFields setting does not reach inside a custom
	// unmarshaler, so unknown keys have to be rejected by hand here. Without
	// this, `filesystem:` inside a host entry would be silently ignored — and a
	// per-host key is the likelier place to make that typo.
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			switch value.Content[i].Value {
			case "host", "name", "filesystems", "services", "containers":
			default:
				return fmt.Errorf("line %d: field %s not found in host entry",
					value.Content[i].Line, value.Content[i].Value)
			}
		}
	}

	// A named alias type avoids recursing back into this method.
	type plain Host
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*h = Host(p)
	return nil
}

// Config is the whole file.
type Config struct {
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	// CommandTimeout bounds a command run across the fleet with x.
	CommandTimeout time.Duration `yaml:"command_timeout"`
	Hosts          []Host        `yaml:"hosts"`
	// Watch lists applied to every host unless that host overrides them.
	//
	// Each is empty by default, and empty means "no filtering": report every
	// filesystem, watch no specific services, show every container. Defaulting
	// any of them to a guess would hide things the operator never chose to
	// hide.
	//
	// Services catches a unit that was stopped rather than one that crashed —
	// systemd does not consider a stopped unit failed, so the failed-unit check
	// alone would show a clean host.
	Filesystems []string `yaml:"filesystems"`
	Services    []string `yaml:"services"`
	Containers  []string `yaml:"containers"`

	// Guests shows LXD instances as rows of their own, nested under the machine
	// running them. On by default because it needs no configuration at all —
	// instances are discovered, not listed — and a host with no LXD installed
	// pays a single `command -v` for the privilege. It is a pointer so that
	// "guests: false" can be told apart from the key being absent.
	Guests *bool `yaml:"guests"`

	Thresholds Thresholds `yaml:"thresholds"`
}

// GuestsEnabled reports whether LXD instances should be discovered.
func (c *Config) GuestsEnabled() bool { return c.Guests == nil || *c.Guests }

// FilesystemsFor returns the mount points to report for a host.
func (c *Config) FilesystemsFor(h Host) []string {
	if len(h.Filesystems) > 0 {
		return h.Filesystems
	}
	return c.Filesystems
}

// ServicesFor returns the units to watch on a host.
func (c *Config) ServicesFor(h Host) []string {
	if len(h.Services) > 0 {
		return h.Services
	}
	return c.Services
}

// ContainersFor returns the containers to watch on a host.
func (c *Config) ContainersFor(h Host) []string {
	if len(h.Containers) > 0 {
		return h.Containers
	}
	return c.Containers
}

// HostRefs converts the configured hosts into the form model.NewFleet wants.
func (c *Config) HostRefs() []model.HostRef {
	refs := make([]model.HostRef, 0, len(c.Hosts))
	for _, h := range c.Hosts {
		refs = append(refs, model.HostRef{
			Name:        h.Name(),
			Addr:        h.Addr,
			Filesystems: c.FilesystemsFor(h),
			Services:    c.ServicesFor(h),
			Containers:  c.ContainersFor(h),
		})
	}
	return refs
}

// DefaultPath is where the config lives absent a -c flag.
//
// This deliberately does not use os.UserConfigDir: on macOS that resolves to
// ~/Library/Application Support, which is not where anyone looks for a
// terminal tool's config and contains spaces that make every shell example
// need quoting. Terminal tools live in ~/.config on macOS as much as on Linux.
func DefaultPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "hmon", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".config", "hmon", "config.yaml")
}

// ErrNotFound signals a missing config file, which main turns into a printed
// example rather than an opaque failure.
var ErrNotFound = errors.New("config file not found")

// Load reads, parses, and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates config bytes. Split out from Load so tests never
// need to touch the filesystem.
func Parse(data []byte) (*Config, error) {
	var c Config

	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Strict decoding turns a typo like `filesystem:` into a startup error
	// instead of a setting that silently does nothing — by far the most
	// annoying class of config bug to track down.
	dec.KnownFields(true)
	// An empty file decodes to EOF rather than an empty struct; fall through so
	// validation reports the useful "no hosts defined" instead of "EOF".
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Interval == 0 {
		c.Interval = DefaultInterval
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = DefaultCommandTimeout
	}
	if c.Thresholds.CPU == (Limits{}) {
		c.Thresholds.CPU = Limits{Warn: 75, Crit: 90}
	}
	if c.Thresholds.Mem == (Limits{}) {
		c.Thresholds.Mem = Limits{Warn: 80, Crit: 92}
	}
	if c.Thresholds.Disk == (Limits{}) {
		c.Thresholds.Disk = Limits{Warn: 80, Crit: 90}
	}
	if c.Thresholds.Temp == (Limits{}) {
		c.Thresholds.Temp = Limits{Warn: 65, Crit: 80}
	}
	// Filesystems is deliberately left empty by default, which means "report
	// every real filesystem". Defaulting it to just "/" would silently hide a
	// filling /mnt/tank from anyone who never edited the setting — the exact
	// failure this tool exists to catch.
}

func (c *Config) validate() error {
	if len(c.Hosts) == 0 {
		return errors.New("config: no hosts defined — add at least one entry under `hosts:`")
	}
	if c.Interval <= 0 {
		return fmt.Errorf("config: interval must be positive, got %s", c.Interval)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("config: timeout must be positive, got %s", c.Timeout)
	}
	if c.CommandTimeout <= 0 {
		return fmt.Errorf("config: command_timeout must be positive, got %s", c.CommandTimeout)
	}
	// Timeout is deliberately allowed to exceed interval. Overlapping polls are
	// already prevented by skipping hosts with a collection in flight, and
	// forcing a sub-interval timeout would make a slow first connection — which
	// still has to complete the full SSH handshake — report as down. A host
	// slower than the interval simply updates less often.

	seen := make(map[string]bool, len(c.Hosts))
	for i, h := range c.Hosts {
		if h.Addr == "" {
			return fmt.Errorf("config: hosts[%d] has no `host:` value", i)
		}
		name := h.Name()
		if seen[name] {
			return fmt.Errorf("config: duplicate host name %q — give one of them a distinct `name:`", name)
		}
		seen[name] = true
	}
	return nil
}
