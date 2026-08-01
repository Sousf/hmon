package collect

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// ExecResult is the outcome of running a script on one host.
type ExecResult struct {
	// Output is stdout and stderr interleaved, in the order the remote shell
	// wrote them. Splitting the two would put a command's errors somewhere
	// other than where they happened, which is exactly the context you need
	// when scanning output from several machines at once.
	Output string
	// ExitCode is the remote command's status. A non-zero exit is a result,
	// not a failure of the tool, so it does not come back as an error.
	ExitCode int
}

// ExecRequest describes one ad-hoc run.
type ExecRequest struct {
	Script string
	// AsRoot runs the whole script under sudo rather than expecting the script
	// to call sudo itself. Doing it once at the top means a multi-line script
	// works without sprinkling sudo through every line, and only one password
	// is needed however many privileged commands it contains.
	AsRoot bool
	// Password is fed to sudo -S as the first line of stdin. It is never placed
	// in the command line, which would expose it to anyone running ps on the
	// host, and never written anywhere.
	Password string
}

// Executor runs an arbitrary script on a host.
type Executor interface {
	Exec(ctx context.Context, addr string, req ExecRequest) (ExecResult, error)
}

// SudoAuthError marks a rejected sudo password. It is distinct from an ssh
// auth failure: the connection worked and the machine is fine, the password
// was simply wrong.
type SudoAuthError struct{}

func (e *SudoAuthError) Error() string { return "sudo password rejected" }

// sudoRejected spots sudo turning the password down. Without this the output
// is three rounds of "Sorry, try again" — sudo retries, consuming the script
// lines as further password attempts — which reads like a broken command
// rather than a bad password.
func sudoRejected(output string) bool {
	return strings.Contains(output, "Sorry, try again") ||
		strings.Contains(output, "incorrect password attempt")
}

// sshSelfError is the exit status ssh uses for its own failures — a refused
// connection, a bad host key — as opposed to relaying the remote command's
// status. It collides with a remote command that genuinely exits 255, which
// is rare enough to accept and impossible to distinguish over the wire.
const sshSelfError = 255

// Exec runs a script on one host and returns its combined output.
//
// The script is piped to the remote shell over stdin rather than passed as
// arguments, which is the same mechanism the collector uses. That means no
// quoting games, no length limit worth worrying about, and a multi-line script
// works exactly like a one-liner.
//
// Execution is non-interactive: BatchMode is set and there is no TTY, so
// anything that tries to prompt — sudo wanting a password, an apt
// confirmation — fails rather than hanging. That is deliberate, since a
// prompt no one can answer would block a whole fan-out.
func (r *SSHRunner) Exec(ctx context.Context, addr string, req ExecRequest) (ExecResult, error) {
	remote, stdin := execRemote(req)

	cmd := exec.CommandContext(ctx, "ssh", append(r.sshBase(addr), remote)...)
	cmd.Stdin = strings.NewReader(stdin)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	res := ExecResult{Output: out.String()}

	if req.AsRoot && sudoRejected(res.Output) {
		// Replace the output rather than pass it through: it is three identical
		// retry lines that say nothing useful, and the script never ran.
		return ExecResult{}, &SudoAuthError{}
	}

	if err == nil {
		return res, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// The deadline fires as a killed process, so check the context before
		// reporting the exit status as though the command chose it.
		if ctx.Err() != nil {
			return res, &TimeoutError{Detail: "command exceeded " + r.timeout.String()}
		}
		if exitErr.ExitCode() == sshSelfError && isAuthFailure(res.Output) {
			return res, &AuthError{Detail: firstLine(res.Output)}
		}
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}

	if ctx.Err() != nil {
		return res, &TimeoutError{Detail: "command exceeded " + r.timeout.String()}
	}
	return res, err
}

// execRemote builds the remote command and the stdin that feeds it.
//
// For a root run, -S makes sudo read the password from stdin, consuming
// exactly one line, and the shell it then execs inherits the remainder as its
// script. -p ” suppresses sudo's prompt, which would otherwise land in the
// captured output.
//
// The password goes over stdin rather than the command line specifically so it
// never appears in the remote process table, where any user on the host could
// read it out of ps.
func execRemote(req ExecRequest) (remote, stdin string) {
	if !req.AsRoot {
		return "sh -s", req.Script
	}
	return "sudo -S -p '' sh -s", req.Password + "\n" + req.Script
}

// ExecRunner adapts an SSHRunner to a longer deadline than polling uses.
//
// Polls are tuned to fail fast so a slow host does not stall the table, but an
// ad-hoc command is something the operator is waiting on deliberately and may
// legitimately take much longer than a poll ever should.
type ExecRunner struct {
	*SSHRunner
}

// NewExecRunner builds a runner for ad-hoc commands with its own timeout.
func NewExecRunner(timeout time.Duration) *ExecRunner {
	return &ExecRunner{SSHRunner: &SSHRunner{timeout: timeout, controlDir: controlDir()}}
}
