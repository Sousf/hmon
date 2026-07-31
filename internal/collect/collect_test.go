package collect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sousf/hmon/internal/model"
)

func TestSSHArgsCarryRequiredOptions(t *testing.T) {
	r := NewSSHRunner(5 * time.Second)
	args := r.sshArgs("nas", nil)
	joined := strings.Join(args, " ")

	// BatchMode is not optional: without it a passphrase-locked key blocks the
	// poll on an invisible prompt, which looks identical to an unreachable host.
	if !strings.Contains(joined, "BatchMode=yes") {
		t.Error("BatchMode=yes missing from ssh args")
	}
	if !strings.Contains(joined, "ConnectTimeout=5") {
		t.Errorf("ConnectTimeout not derived from timeout: %s", joined)
	}

	// The destination must precede the remote command.
	if got, want := args[len(args)-2], "nas"; got != want {
		t.Errorf("destination = %q, want %q", got, want)
	}
	if got, want := args[len(args)-1], "sh -s"; got != want {
		t.Errorf("remote command = %q, want %q", got, want)
	}
}

func TestSSHArgsRequestProcs(t *testing.T) {
	r := NewSSHRunner(time.Second)
	args := r.sshArgs("nas", []string{"procs"})
	if got, want := args[len(args)-1], "sh -s -- procs"; got != want {
		t.Errorf("remote command = %q, want %q", got, want)
	}
}

func TestOptsArgs(t *testing.T) {
	// No options means no extra work on the remote side.
	if got := (Opts{}).args(); len(got) != 0 {
		t.Errorf("empty Opts produced args %v, want none", got)
	}

	got := strings.Join(Opts{Detail: true, Services: []string{"caddy", "postgresql"}}.args(), " ")
	for _, want := range []string{"procs", "containers", "svc=caddy,postgresql"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}

	// Services are cheap, so they are requested without the detail sections.
	got = strings.Join(Opts{Services: []string{"ssh"}}.args(), " ")
	if strings.Contains(got, "procs") {
		t.Errorf("args %q requested procs without Detail", got)
	}
}

func TestConnectTimeoutNeverZero(t *testing.T) {
	// A sub-second timeout truncates to 0, and ConnectTimeout=0 means "no
	// timeout" to ssh — the opposite of what was asked for.
	r := NewSSHRunner(300 * time.Millisecond)
	joined := strings.Join(r.sshArgs("nas", nil), " ")
	if strings.Contains(joined, "ConnectTimeout=0") {
		t.Errorf("ConnectTimeout=0 disables the timeout entirely: %s", joined)
	}
	if !strings.Contains(joined, "ConnectTimeout=1") {
		t.Errorf("want ConnectTimeout clamped to 1, got: %s", joined)
	}
}

// TestControlPathFitsSocketLimit covers a failure that took a live SSH
// connection to surface: ssh refuses any ControlPath that will not fit in
// sockaddr_un.sun_path (104 bytes on macOS and BSD), and when it refuses,
// every single poll fails.
func TestControlPathFitsSocketLimit(t *testing.T) {
	r := NewSSHRunner(time.Second)
	for _, addr := range []string{
		"nas",
		"ssh://monitor@192.168.1.100:22022",
		strings.Repeat("very-long-hostname", 10),
	} {
		cp := r.controlPath(addr)
		if cp == "" {
			continue // multiplexing disabled, which is a valid outcome
		}
		if len(cp) > maxControlPath {
			t.Errorf("controlPath(%q) is %d bytes, over the %d limit: %s",
				addr, len(cp), maxControlPath, cp)
		}
	}
}

func TestControlPathIsStablePerHostAndDistinctAcrossHosts(t *testing.T) {
	r := NewSSHRunner(time.Second)
	a1, a2 := r.controlPath("nas"), r.controlPath("nas")
	if a1 != a2 {
		// An unstable path would open a fresh master connection every poll,
		// silently defeating multiplexing.
		t.Errorf("controlPath not stable: %q then %q", a1, a2)
	}
	if b := r.controlPath("proxmox"); a1 == b {
		t.Errorf("distinct hosts share a control socket: %q", b)
	}
}

func TestOmitsMultiplexingWhenNoPathFits(t *testing.T) {
	r := &SSHRunner{timeout: time.Second, controlDir: ""}
	joined := strings.Join(r.sshArgs("nas", nil), " ")
	if strings.Contains(joined, "ControlPath") {
		t.Errorf("ControlPath present with no control dir: %s", joined)
	}
	// Polls must still work, just without connection reuse.
	if !strings.Contains(joined, "BatchMode=yes") {
		t.Error("dropped the rest of the options along with multiplexing")
	}
}

func TestClassifyDistinguishesFailureKinds(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want model.FailKind
	}{
		{"auth", &AuthError{Detail: "Permission denied (publickey)"}, model.FailAuth},
		{"bad output", ErrBadOutput, model.FailBadOutput},
		{"wrapped bad output", errors.New("x: " + ErrBadOutput.Error()), model.FailUnreachable},
		{"unreachable", errors.New("connection refused"), model.FailUnreachable},
		{"timeout", &TimeoutError{Detail: "no response"}, model.FailUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsAuthFailureRecognisesCommonMessages(t *testing.T) {
	positive := []string{
		"monitor@nas: Permission denied (publickey).",
		"Host key verification failed.",
		"No supported authentication methods available",
	}
	for _, s := range positive {
		if !isAuthFailure(s) {
			t.Errorf("isAuthFailure(%q) = false, want true", s)
		}
	}

	negative := []string{
		"ssh: connect to host nas port 22: Connection refused",
		"ssh: Could not resolve hostname nas",
		"",
	}
	for _, s := range negative {
		if isAuthFailure(s) {
			t.Errorf("isAuthFailure(%q) = true, want false", s)
		}
	}
}

// fakeRunner returns canned output, standing in for SSH.
type fakeRunner struct {
	out  []byte
	err  error
	args []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, args []string) ([]byte, error) {
	f.args = args
	return f.out, f.err
}

func TestPollParsesRunnerOutput(t *testing.T) {
	fr := &fakeRunner{out: []byte("v 1\nmem 1000 400\nend\n")}
	p := &Poller{Runner: fr, Timeout: time.Second}

	s, err := p.Poll(context.Background(), "nas", Opts{})
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if !s.HasMem {
		t.Error("HasMem = false, want true")
	}
	if len(fr.args) != 0 {
		t.Errorf("args = %v, want none when procs not requested", fr.args)
	}
}

func TestPollRequestsProcsWhenAsked(t *testing.T) {
	fr := &fakeRunner{out: []byte("v 1\nend\n")}
	p := &Poller{Runner: fr, Timeout: time.Second}

	if _, err := p.Poll(context.Background(), "nas", Opts{Detail: true}); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	joined := strings.Join(fr.args, " ")
	if !strings.Contains(joined, "procs") || !strings.Contains(joined, "containers") {
		t.Errorf("args = %v, want procs and containers", fr.args)
	}
}

func TestPollPropagatesRunnerError(t *testing.T) {
	fr := &fakeRunner{err: &AuthError{Detail: "denied"}}
	p := &Poller{Runner: fr, Timeout: time.Second}

	_, err := p.Poll(context.Background(), "nas", Opts{})
	if err == nil {
		t.Fatal("Poll() succeeded, want error")
	}
	if got := Classify(err); got != model.FailAuth {
		t.Errorf("Classify() = %v, want FailAuth", got)
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct{ in, want string }{
		{"one\ntwo\nthree", "one"},
		{"  spaced  \nnext", "spaced"},
		{"single", "single"},
		{"", ""},
		{"\n\n", ""},
	}
	for _, tt := range tests {
		if got := firstLine(tt.in); got != tt.want {
			t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestCollectorScriptEmbedded guards against the script going missing from the
// binary, which would make every host report bad output.
func TestCollectorScriptEmbedded(t *testing.T) {
	if !strings.Contains(collectorScript, "echo \"v "+FormatVersion+"\"") {
		t.Errorf("embedded collector does not emit format version %s", FormatVersion)
	}
	if !strings.Contains(collectorScript, "/proc/meminfo") {
		t.Error("embedded collector missing memory collection")
	}
}
