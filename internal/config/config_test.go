package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseHostFormsAndDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
hosts:
  - nas
  - host: media-01.lan
    name: media
    filesystems: [/, /mnt/media]
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if got, want := len(cfg.Hosts), 2; got != want {
		t.Fatalf("host count = %d, want %d", got, want)
	}

	// Scalar form is sugar for {host: X}, with the display name falling back to
	// the ssh target.
	if got, want := cfg.Hosts[0].Addr, "nas"; got != want {
		t.Errorf("scalar host addr = %q, want %q", got, want)
	}
	if got, want := cfg.Hosts[0].Name(), "nas"; got != want {
		t.Errorf("scalar host name = %q, want %q", got, want)
	}
	// Empty filesystems means "report everything", not "report /".
	if got := cfg.Hosts[0].Filesystems; len(got) != 0 {
		t.Errorf("scalar host filesystems = %v, want empty", got)
	}

	if got, want := cfg.Hosts[1].Addr, "media-01.lan"; got != want {
		t.Errorf("mapping host addr = %q, want %q", got, want)
	}
	if got, want := cfg.Hosts[1].Name(), "media"; got != want {
		t.Errorf("mapping host name = %q, want %q", got, want)
	}

	if got, want := cfg.Interval, DefaultInterval; got != want {
		t.Errorf("interval = %v, want %v", got, want)
	}
	if got, want := cfg.Timeout, DefaultTimeout; got != want {
		t.Errorf("timeout = %v, want %v", got, want)
	}
	if got, want := cfg.Thresholds.CPU.Warn, 75.0; got != want {
		t.Errorf("default cpu warn = %v, want %v", got, want)
	}
}

func TestParseExplicitValues(t *testing.T) {
	cfg, err := Parse([]byte(`
interval: 10s
timeout: 3s
hosts: [a, b]
thresholds:
  cpu: {warn: 50, crit: 60}
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := cfg.Interval, 10*time.Second; got != want {
		t.Errorf("interval = %v, want %v", got, want)
	}
	if got, want := cfg.Thresholds.CPU.Crit, 60.0; got != want {
		t.Errorf("cpu crit = %v, want %v", got, want)
	}
	if got, want := cfg.Timeout, 3*time.Second; got != want {
		t.Errorf("timeout = %v, want %v", got, want)
	}
	// An explicitly set section must not be overwritten by defaults, while
	// untouched sections still get them.
	if got, want := cfg.Thresholds.Mem.Warn, 80.0; got != want {
		t.Errorf("mem warn = %v, want %v", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "no hosts",
			yaml:    "interval: 2s\n",
			wantErr: "no hosts defined",
		},
		{
			name:    "empty file",
			yaml:    "",
			wantErr: "no hosts defined",
		},
		{
			// The whole point of strict decoding: a plausible typo must fail
			// loudly instead of silently doing nothing.
			name:    "unknown field",
			yaml:    "hosts: [a]\nfilesystem: /\n",
			wantErr: "field filesystem not found",
		},
		{
			name:    "unknown per-host field",
			yaml:    "hosts:\n  - host: a\n    filesystem: /\n",
			wantErr: "field filesystem not found",
		},
		{
			name:    "negative interval",
			yaml:    "interval: -2s\nhosts: [a]\n",
			wantErr: "interval must be positive",
		},
		{
			name:    "duplicate names",
			yaml:    "hosts:\n  - a\n  - host: b\n    name: a\n",
			wantErr: "duplicate host name",
		},
		{
			name:    "host without addr",
			yaml:    "hosts:\n  - name: only-a-label\n",
			wantErr: "has no `host:` value",
		},
		{
			name:    "bad duration",
			yaml:    "interval: soon\nhosts: [a]\n",
			wantErr: "parsing config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("Parse() succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Parse() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLimitsClassify(t *testing.T) {
	l := Limits{Warn: 80, Crit: 90}
	tests := []struct {
		v    float64
		want Level
	}{
		{0, LevelOK},
		{79.9, LevelOK},
		{80, LevelWarn},
		{89.9, LevelWarn},
		{90, LevelCrit},
		{100, LevelCrit},
	}
	for _, tt := range tests {
		if got := l.Classify(tt.v); got != tt.want {
			t.Errorf("Classify(%v) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestDefaultPathUsesDotConfig(t *testing.T) {
	// os.UserConfigDir would give ~/Library/Application Support on macOS, which
	// is neither where a terminal tool belongs nor safe to paste into a shell
	// unquoted.
	t.Setenv("XDG_CONFIG_HOME", "")
	got := DefaultPath()
	if strings.Contains(got, "Application Support") {
		t.Errorf("DefaultPath() = %q, want a path under .config", got)
	}
	if !strings.Contains(got, filepath.Join(".config", "hmon", "config.yaml")) {
		t.Errorf("DefaultPath() = %q, want it under .config/hmon", got)
	}
}

func TestDefaultPathHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/cfg")
	if got, want := DefaultPath(), "/custom/cfg/hmon/config.yaml"; got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestHostRefsCarryFilesystems(t *testing.T) {
	cfg, err := Parse([]byte("hosts:\n  - host: a\n    filesystems: [/, /mnt/x]\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	refs := cfg.HostRefs()
	if len(refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(refs))
	}
	if got, want := len(refs[0].Filesystems), 2; got != want {
		t.Errorf("filesystems = %d, want %d", got, want)
	}
}
