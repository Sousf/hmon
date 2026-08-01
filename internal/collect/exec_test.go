package collect

import (
	"strings"
	"testing"
)

func TestExecRemotePlain(t *testing.T) {
	remote, stdin := execRemote(ExecRequest{Script: "uptime"})
	if remote != "sh -s" {
		t.Errorf("remote = %q, want %q", remote, "sh -s")
	}
	if stdin != "uptime" {
		t.Errorf("stdin = %q, want the script verbatim", stdin)
	}
}

// TestExecRemoteRootPutsPasswordOnStdinNotArgv is the security property: the
// password must never reach the command line, where any user on the host could
// read it out of ps.
func TestExecRemoteRootPutsPasswordOnStdinNotArgv(t *testing.T) {
	req := ExecRequest{Script: "apt update\napt upgrade -y", AsRoot: true, Password: "hunter2"}
	remote, stdin := execRemote(req)

	if strings.Contains(remote, "hunter2") {
		t.Fatalf("password present in the remote command line: %q", remote)
	}
	if !strings.Contains(remote, "sudo -S") {
		t.Errorf("remote = %q, want it to run under sudo -S", remote)
	}
	if !strings.Contains(remote, "-p ''") {
		t.Errorf("remote = %q, want the sudo prompt suppressed", remote)
	}

	// Exactly one line of password, then the script — that split is what makes
	// sudo consume the secret and the shell receive the rest.
	first, rest, found := strings.Cut(stdin, "\n")
	if !found {
		t.Fatalf("stdin has no newline to split on: %q", stdin)
	}
	if first != "hunter2" {
		t.Errorf("first stdin line = %q, want the password alone", first)
	}
	if rest != req.Script {
		t.Errorf("remaining stdin = %q, want the script verbatim", rest)
	}
}

func TestSudoRejectedRecognisesFailure(t *testing.T) {
	for _, out := range []string{
		"Sorry, try again.\n",
		"sudo: 3 incorrect password attempts\n",
	} {
		if !sudoRejected(out) {
			t.Errorf("sudoRejected(%q) = false, want true", out)
		}
	}
	for _, out := range []string{
		"root\n",
		"",
		"apt: command not found\n",
	} {
		if sudoRejected(out) {
			t.Errorf("sudoRejected(%q) = true, want false", out)
		}
	}
}
