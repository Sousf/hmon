// Package collect polls hosts over SSH and turns their output into samples.
//
// Collection is deliberately stateless: raw counters go over the wire and any
// value needing two points in time is derived later by the model. That keeps
// this package a pure function of (host, output) and lets every test run
// without a network.
package collect

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sousf/hmon/internal/model"
)

// collectorScript is piped to the remote shell's stdin on every poll, so
// nothing is ever installed on the monitored machines.
//
//go:embed collector.sh
var collectorScript string

// Runner executes the collector on a host and returns its stdout. It exists so
// tests can inject fixture output instead of reaching for the network — the
// single seam that makes this package testable.
type Runner interface {
	Run(ctx context.Context, addr string, args []string) (stdout []byte, err error)
}

// Poller collects samples from hosts.
type Poller struct {
	Runner  Runner
	Timeout time.Duration
}

// New builds a Poller using real SSH.
func New(timeout time.Duration) *Poller {
	return &Poller{Runner: NewSSHRunner(timeout), Timeout: timeout}
}

// Opts selects which optional sections a poll collects.
type Opts struct {
	// Detail turns on the expensive per-host sections — top processes and
	// container listings. Both cost real time on the remote side, so they are
	// only requested for the host whose detail is on screen.
	Detail bool
	// Containers requests the container listing on its own. It is needed for
	// every host with a watch list, not just the one on screen: a health flag
	// driven by data collected only for the selected host would be wrong for
	// every other row.
	Containers bool
	// Services are unit names to check. One systemctl call covers the whole
	// list, so this is cheap enough to run for every host on every poll.
	Services []string
	// Guests discovers LXD instances and measures each from the inside. Like
	// containers this is a fleet-wide signal rather than a detail-only one:
	// guests occupy rows of their own, so every host needs them every poll.
	Guests bool
	// GuestProcs names the one instance whose top processes are also wanted,
	// which is the guest currently being viewed in detail. Empty otherwise, for
	// the same reason hosts only collect processes for the selected row.
	GuestProcs string
}

func (o Opts) args() []string {
	var args []string
	if o.Detail {
		args = append(args, "procs")
	}
	if o.Detail || o.Containers {
		args = append(args, "containers")
	}
	if o.Guests {
		args = append(args, "guests")
		if o.GuestProcs != "" {
			args = append(args, "gprocs="+o.GuestProcs)
		}
	}
	if len(o.Services) > 0 {
		args = append(args, "svc="+strings.Join(o.Services, ","))
	}
	return args
}

// guestsArg is the flag whose presence means the collector may need to probe
// inside an LXD instance, and so needs a copy of itself to pipe onward.
const guestsArg = "guests"

// remoteInput is what gets piped to the remote shell.
//
// When guests are wanted the collector is preceded by a copy of itself, quoted
// into a variable. That is what lets an LXD instance be measured by the very
// same code as the machine hosting it, rather than by a second, lesser probe
// that would have to duplicate the awkward parts — the two-sample process
// accounting, the /proc/net/dev column handling — and drift out of step with
// them. The remote shell never sees the copy as code, only as a string it
// passes on, so there is no recursion: the inner run is invoked with "guest",
// never "guests".
func remoteInput(args []string) string {
	wantGuests := false
	for _, a := range args {
		if a == guestsArg {
			wantGuests = true
			break
		}
	}
	if !wantGuests {
		return collectorScript
	}
	return "HMON_GUEST_PROBE=" + shellQuote(collectorScript) + "\n" + collectorScript
}

// shellQuote wraps s so a POSIX shell reproduces it byte for byte. Single
// quotes suppress every form of expansion, so the only character needing
// special handling is the single quote itself, which is closed, escaped, and
// reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Poll runs one collection against a host.
func (p *Poller) Poll(ctx context.Context, addr string, opts Opts) (model.Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	out, err := p.Runner.Run(ctx, addr, opts.args())
	if err != nil {
		return model.Sample{}, err
	}
	return Parse(out, time.Now())
}

// Classify maps a poll error onto the failure kind the UI reacts to. The
// distinction matters: an unreachable host is routine and worth retrying,
// while rejected credentials are a configuration problem that retrying will
// never fix.
func Classify(err error) model.FailKind {
	if err == nil {
		return model.FailUnreachable
	}
	if errors.Is(err, ErrBadOutput) {
		return model.FailBadOutput
	}

	var authErr *AuthError
	if errors.As(err, &authErr) {
		return model.FailAuth
	}
	return model.FailUnreachable
}

// AuthError marks an SSH failure caused by rejected credentials.
type AuthError struct{ Detail string }

func (e *AuthError) Error() string { return "authentication failed: " + e.Detail }

// SSHRunner shells out to the system ssh client.
//
// Using the real ssh binary rather than a Go SSH library is a deliberate
// choice: it means ~/.ssh/config already supplies host aliases, identity
// files, ports, and ProxyJump, so a host in our own config is usually just a
// name. Reimplementing that lookup would be a large surface area for a tool
// whose whole point is staying small.
type SSHRunner struct {
	timeout time.Duration

	// Directory holding the multiplexing sockets. The first poll performs the
	// handshake and later polls reuse the socket, which is what keeps a
	// subprocess-per-poll cheap enough to be a non-issue at this fleet size.
	// It is monitor-specific so these never collide with the user's own
	// interactive sessions and can be torn down independently on quit.
	controlDir string
}

// maxControlPath is the ceiling ssh enforces on a ControlPath. It is not
// arbitrary: the socket path has to fit in sockaddr_un.sun_path, which is 104
// bytes on macOS and BSD. Exceeding it makes ssh refuse to connect at all, so
// every poll would fail.
const maxControlPath = 100

// maxConnectSecs bounds how long ssh waits to establish a connection,
// regardless of how long the command it carries is allowed to take.
const maxConnectSecs = 30

// NewSSHRunner builds a runner with connection multiplexing enabled.
func NewSSHRunner(timeout time.Duration) *SSHRunner {
	return &SSHRunner{timeout: timeout, controlDir: controlDir()}
}

// controlDir picks a directory for multiplexing sockets that leaves room under
// the sun_path limit. The cache directory is preferred, but a long home path
// (a long username is enough) can push it over, so fall back to a short path
// under /tmp rather than failing every connection.
func controlDir() string {
	candidates := []string{}
	if cache, err := os.UserCacheDir(); err == nil {
		candidates = append(candidates, filepath.Join(cache, "hmon", "cm"))
	}
	candidates = append(candidates,
		filepath.Join(os.TempDir(), fmt.Sprintf("hmon-%d", os.Getuid())),
		fmt.Sprintf("/tmp/hmon-%d", os.Getuid()),
	)

	for _, dir := range candidates {
		// Reserve room for the socket filename appended later.
		if len(dir)+1+socketNameLen > maxControlPath {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			continue
		}
		return dir
	}
	// Nothing fit. Returning empty disables multiplexing rather than breaking
	// every connection: polls still work, they just pay a full handshake each
	// time.
	return ""
}

// socketNameLen is the length of the hashed socket filename.
const socketNameLen = 12

// controlPath returns the multiplexing socket for one host. The name is a
// short hash of the destination rather than ssh's %C token, which expands to
// 40 hex characters and makes the total path far more likely to overflow.
func (r *SSHRunner) controlPath(addr string) string {
	if r.controlDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(addr))
	return filepath.Join(r.controlDir, hex.EncodeToString(sum[:])[:socketNameLen])
}

func (r *SSHRunner) sshArgs(addr string, args []string) []string {
	out := r.sshBase(addr)

	remote := "sh -s"
	if len(args) > 0 {
		remote += " -- " + strings.Join(args, " ")
	}
	return append(out, remote)
}

// sshBase builds the connection options and destination, without a remote
// command. Shared by polling and ad-hoc execution so both get the same
// multiplexing, timeouts, and non-interactive behaviour.
func (r *SSHRunner) sshBase(addr string) []string {
	connectSecs := int(r.timeout.Seconds())
	if connectSecs < 1 {
		connectSecs = 1
	}
	// Capped independently of the command budget. A launch is allowed ten
	// minutes because it may be downloading an image, but that is time granted
	// to work already under way — spending it waiting for a dead host to
	// complete a handshake would just hide the failure for ten minutes.
	if connectSecs > maxConnectSecs {
		connectSecs = maxConnectSecs
	}

	out := []string{
		// Never prompt. Without this a passphrase-locked key blocks the poll on
		// an invisible prompt until timeout, which looks like an unreachable
		// host and hides the real problem.
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=" + strconv.Itoa(connectSecs),
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=1",
	}

	// Multiplexing is a performance optimisation, so skip it rather than fail
	// when no short enough socket path could be found.
	if cp := r.controlPath(addr); cp != "" {
		out = append(out,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+cp,
			"-o", "ControlPersist=60s",
		)
	}
	return append(out, addr)
}

// Run pipes the collector script to the host's shell and returns its stdout.
func (r *SSHRunner) Run(ctx context.Context, addr string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", r.sshArgs(addr, args)...)
	cmd.Stdin = strings.NewReader(remoteInput(args))

	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		detail := firstLine(stderr.String())
		if isAuthFailure(stderr.String()) {
			return nil, &AuthError{Detail: detail}
		}
		if ctx.Err() != nil {
			return nil, &TimeoutError{Detail: "no response within " + r.timeout.String()}
		}
		if detail == "" {
			detail = err.Error()
		}
		return nil, errors.New(detail)
	}
	return out, nil
}

// TimeoutError marks a poll that exceeded its deadline.
type TimeoutError struct{ Detail string }

func (e *TimeoutError) Error() string { return e.Detail }

// Close tears down the multiplexed master connection for a host, so quitting
// does not leave orphaned ssh processes behind.
func (r *SSHRunner) Close(addr string) {
	cp := r.controlPath(addr)
	if cp == "" {
		return // multiplexing was never enabled; nothing to tear down
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "ControlPath="+cp, "-O", "exit", addr)
	_ = cmd.Run()
}

func isAuthFailure(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, marker := range []string{
		"permission denied",
		"authentication failed",
		"no supported authentication methods",
		"host key verification failed",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
