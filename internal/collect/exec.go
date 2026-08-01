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

// Executor runs an arbitrary script on a host.
type Executor interface {
	Exec(ctx context.Context, addr, script string) (ExecResult, error)
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
func (r *SSHRunner) Exec(ctx context.Context, addr, script string) (ExecResult, error) {
	cmd := exec.CommandContext(ctx, "ssh", append(r.sshBase(addr), "sh -s")...)
	cmd.Stdin = strings.NewReader(script)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	res := ExecResult{Output: out.String()}

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
