package collect

import (
	"errors"
	"strings"
	"testing"
)

func mustScript(t *testing.T, req LaunchRequest) string {
	t.Helper()
	s, err := LaunchScript(req)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLaunchScriptBuildsTheCommand(t *testing.T) {
	s := mustScript(t, LaunchRequest{
		Name: "build-01", Image: "images:nixos/25.05/cloud",
		VM: true, CPU: 4, Memory: "4GiB",
	})

	want := "lxc launch 'images:nixos/25.05/cloud' build-01 --vm -c limits.cpu=4 -c limits.memory='4GiB'"
	if !strings.Contains(s, want) {
		t.Errorf("script does not contain\n  %s\ngot:\n%s", want, s)
	}
	// The confirmation screen shows this same line, so the two must agree.
	if cmd := LaunchCommand(LaunchRequest{
		Name: "build-01", Image: "images:nixos/25.05/cloud",
		VM: true, CPU: 4, Memory: "4GiB",
	}); cmd != want {
		t.Errorf("LaunchCommand = %q, want %q", cmd, want)
	}
}

// Unset limits are left off entirely rather than sent as zero, so the instance
// inherits whatever its profile grants.
func TestLaunchScriptOmitsUnsetLimits(t *testing.T) {
	s := mustScript(t, LaunchRequest{Name: "web", Image: "ubuntu:24.04"})

	if !strings.Contains(s, "lxc launch 'ubuntu:24.04' web\n") {
		t.Errorf("expected a bare launch line, got:\n%s", s)
	}
	for _, unwanted := range []string{"--vm", "limits.cpu", "limits.memory"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("script mentions %q for a template that did not ask for it", unwanted)
		}
	}
}

// Nothing to run inside means nothing to wait for. Waiting anyway would stall
// for minutes on any image with no lxd-agent, then call a healthy instance a
// failure.
func TestLaunchScriptSkipsTheWaitWithoutProvisioning(t *testing.T) {
	s := mustScript(t, LaunchRequest{Name: "web", Image: "ubuntu:24.04"})

	for _, unwanted := range []string{"lxc exec", "PROVISION", "until"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("script waits or provisions when it has no script to run:\n%s", s)
			break
		}
	}
	if !strings.Contains(s, "web created") {
		t.Error("no completion message")
	}
}

func TestLaunchScriptWaitsThenProvisions(t *testing.T) {
	s := mustScript(t, LaunchRequest{
		Name: "web", Image: "ubuntu:24.04",
		Provision: "echo hi\n",
	})

	waitAt := strings.Index(s, "until lxc exec web -- true")
	provisionAt := strings.Index(s, `printf '%s\n' "$PROVISION" | lxc exec web -- sh -s`)
	launchAt := strings.Index(s, "lxc launch")
	if launchAt < 0 || waitAt < 0 || provisionAt < 0 {
		t.Fatalf("missing a stage:\n%s", s)
	}
	// Order is the whole contract: provisioning an instance that has not
	// answered yet fails for a reason that has nothing to do with the script.
	if !(launchAt < waitAt && waitAt < provisionAt) {
		t.Errorf("stages out of order: launch=%d wait=%d provision=%d", launchAt, waitAt, provisionAt)
	}

	// A failed provision must not destroy the instance. It is running and
	// reachable, and deleting a machine over a non-zero exit is not a decision
	// to make for someone.
	if strings.Contains(s, "lxc delete") {
		t.Error("script deletes the instance on failure")
	}
	if !strings.Contains(s, "was created and is running, but provisioning failed") {
		t.Error("failure is not reported as a surviving instance")
	}
}

// The pool check runs before anything is created; LXD only reports a missing
// pool after it has already begun work.
func TestLaunchScriptChecksReadinessFirst(t *testing.T) {
	s := mustScript(t, LaunchRequest{Name: "web", Image: "ubuntu:24.04"})

	poolAt := strings.Index(s, "lxc storage list")
	existsAt := strings.Index(s, "lxc info web")
	launchAt := strings.Index(s, "lxc launch")
	if poolAt < 0 || existsAt < 0 {
		t.Fatalf("missing a precondition:\n%s", s)
	}
	if !(poolAt < launchAt && existsAt < launchAt) {
		t.Errorf("preconditions run after the launch: pool=%d exists=%d launch=%d",
			poolAt, existsAt, launchAt)
	}
}

// A provision script containing quotes must survive being embedded, or the
// variable assignment closes early and the rest runs as commands.
func TestProvisionScriptSurvivesQuoting(t *testing.T) {
	provision := "echo 'single' \"double\"\nexit 0\n"
	s := mustScript(t, LaunchRequest{
		Name: "web", Image: "ubuntu:24.04", Provision: provision,
	})

	start := strings.Index(s, "PROVISION='")
	if start < 0 {
		t.Fatal("no PROVISION assignment")
	}
	body := s[start+len("PROVISION='"):]
	body = body[:strings.Index(body, "'\n\necho")]
	if got := strings.ReplaceAll(body, `'\''`, "'"); got != provision {
		t.Errorf("round trip = %q, want %q", got, provision)
	}
}

// The name is interpolated unquoted because it has been validated, so building
// a script for an unvalidated name has to be impossible rather than merely
// discouraged.
func TestLaunchScriptRefusesUnsafeInput(t *testing.T) {
	_, err := LaunchScript(LaunchRequest{Name: "web; rm -rf /", Image: "ubuntu:24.04"})
	if !errors.Is(err, ErrBadInstanceName) {
		t.Errorf("err = %v, want ErrBadInstanceName", err)
	}

	if _, err := LaunchScript(LaunchRequest{Name: "web"}); err == nil {
		t.Error("a launch with no image was accepted")
	}
}

func TestValidInstanceName(t *testing.T) {
	valid := []string{"web", "build-01", "a", "x1", "nixos-test-2"}
	for _, n := range valid {
		if err := ValidInstanceName(n); err != nil {
			t.Errorf("ValidInstanceName(%q) = %v, want nil", n, err)
		}
	}

	invalid := []string{
		"",            // empty
		"-web",        // leading hyphen
		"web-",        // trailing hyphen
		"123",         // all digits
		"web_01",      // underscore
		"web.example", // dot
		"web 01",      // space
		"web;rm",      // shell metacharacter
		"WÉB",         // non-ascii
		strings.Repeat("a", maxInstanceName+1),
	}
	for _, n := range invalid {
		if err := ValidInstanceName(n); err == nil {
			t.Errorf("ValidInstanceName(%q) = nil, want an error", n)
		}
	}

	// Exactly at the limit is fine; one past it is not.
	if err := ValidInstanceName(strings.Repeat("a", maxInstanceName)); err != nil {
		t.Errorf("a %d-character name was refused: %v", maxInstanceName, err)
	}
}

// A ten-minute launch budget must not become a ten-minute wait on a host that
// is simply not there.
func TestConnectTimeoutIsCappedIndependentlyOfTheCommandBudget(t *testing.T) {
	r := NewExecRunner(10 * 60 * 1e9) // 10 minutes
	args := strings.Join(r.sshBase("host"), " ")
	if !strings.Contains(args, "ConnectTimeout=30") {
		t.Errorf("ConnectTimeout not capped: %s", args)
	}
}
