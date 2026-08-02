package collect

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// LaunchRequest describes one instance to create on a host.
type LaunchRequest struct {
	Name  string
	Image string
	// VM selects a virtual machine over a system container.
	VM bool
	// CPU and Memory become limits.cpu and limits.memory. Both are optional and
	// are left off the command entirely when unset, so the instance inherits
	// whatever its profile grants.
	CPU    int
	Memory string
	// Provision is the contents of a script to run inside the instance once it
	// answers. Empty means the launch is done as soon as LXD returns.
	Provision string
}

// readyAttempts and readyInterval bound the wait for a new instance to answer.
// Three minutes is generous for a container, which responds almost at once, and
// realistic for a VM that has to boot far enough to start its lxd-agent.
const (
	readyAttempts = 90
	readyInterval = 2
)

// LaunchScript builds the shell script that creates and provisions an instance.
//
// A launch is deliberately expressed as one ad-hoc script rather than as a new
// transport: hmon already knows how to pipe a script to a host over ssh with a
// long deadline and collect its output, and how to pipe a script onward into an
// instance with `lxc exec`. This is those two things in sequence, so the whole
// feature is the text below.
//
// Output is narrated with "==>" markers because a launch is slow enough that
// the results view would otherwise sit blank for minutes: knowing whether it is
// downloading an image or waiting on a boot is the difference between waiting
// and pulling the plug.
//
// The name is validated rather than quoted. It ends up in echo text as well as
// in arguments, and a name that has passed ValidInstanceName holds nothing a
// shell would look at twice — which is a stronger guarantee than remembering to
// quote it in six places.
func LaunchScript(req LaunchRequest) (string, error) {
	if err := ValidInstanceName(req.Name); err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadInstanceName, err)
	}
	if req.Image == "" {
		return "", errors.New("launch: no image given")
	}

	var b strings.Builder
	p := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
	}

	p("set -eu\n\n")

	// Checked before anything is created. LXD reports a missing storage pool
	// only after it has already begun work, and names an internal step rather
	// than the thing you actually have to fix.
	p("if ! lxc storage list --format csv 2>/dev/null | grep -q .; then\n")
	p("  echo \"hmon: LXD has no storage pool on this host — run 'lxc init' first\" >&2\n")
	p("  exit 1\n")
	p("fi\n\n")

	p("if lxc info %s >/dev/null 2>&1; then\n", req.Name)
	p("  echo \"hmon: an instance called %s already exists here\" >&2\n", req.Name)
	p("  exit 1\n")
	p("fi\n\n")

	p("echo \"==> launching %s\"\n", req.Name)
	p("%s\n", LaunchCommand(req))

	if req.Provision == "" {
		// Nothing to run inside, so nothing to wait for. Waiting anyway would
		// stall for three minutes on any image without an lxd-agent, then report
		// a failure for an instance that started perfectly well.
		p("\necho \"==> %s created\"\n", req.Name)
		return b.String(), nil
	}

	p("\nPROVISION=%s\n\n", shellQuote(req.Provision))
	p("echo \"==> waiting for %s to answer\"\n", req.Name)
	p("n=0\n")
	p("until lxc exec %s -- true 2>/dev/null; do\n", req.Name)
	p("  n=$((n + 1))\n")
	p("  if [ \"$n\" -ge %d ]; then\n", readyAttempts)
	p("    echo \"hmon: %s was created but never answered\" >&2\n", req.Name)
	p("    exit 1\n")
	p("  fi\n")
	p("  sleep %d\n", readyInterval)
	p("done\n\n")

	// A failed provision leaves the instance running and says so. Deleting it
	// would destroy a machine because a script exited non-zero, which is not a
	// call to make on the operator's behalf — it is reachable and can be looked
	// at.
	p("echo \"==> provisioning %s\"\n", req.Name)
	p("if printf '%%s\\n' \"$PROVISION\" | lxc exec %s -- sh -s; then\n", req.Name)
	p("  echo \"==> %s is ready\"\n", req.Name)
	p("else\n")
	p("  echo \"hmon: %s was created and is running, but provisioning failed\" >&2\n", req.Name)
	p("  exit 1\n")
	p("fi\n")

	return b.String(), nil
}

// LaunchCommand is the `lxc launch` line on its own.
//
// Shared with the confirmation screen rather than reconstructed there, so what
// you are shown before agreeing is the command that actually runs — the one
// place where two implementations drifting apart would be a genuine hazard.
func LaunchCommand(req LaunchRequest) string {
	var b strings.Builder
	b.WriteString("lxc launch " + shellQuote(req.Image) + " " + req.Name)
	if req.VM {
		b.WriteString(" --vm")
	}
	if req.CPU > 0 {
		b.WriteString(" -c limits.cpu=" + strconv.Itoa(req.CPU))
	}
	if req.Memory != "" {
		b.WriteString(" -c limits.memory=" + shellQuote(req.Memory))
	}
	return b.String()
}

// ErrBadInstanceName marks a name LXD would refuse.
var ErrBadInstanceName = errors.New("invalid instance name")

// maxInstanceName is LXD's ceiling, which comes from the DNS label limit —
// instance names become hostnames on the bridge.
const maxInstanceName = 63

// ValidInstanceName applies LXD's naming rules locally, so a typo is caught
// while you are still typing rather than after a round trip to the host.
//
// The rules are DNS label rules: letters, digits and hyphens only, no leading
// or trailing hyphen, and never all digits.
func ValidInstanceName(name string) error {
	switch {
	case name == "":
		return errors.New("name is empty")
	case len(name) > maxInstanceName:
		return errors.New("name is longer than " + strconv.Itoa(maxInstanceName) + " characters")
	case strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-"):
		return errors.New("name cannot start or end with a hyphen")
	}

	allDigits := true
	for _, r := range name {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-':
			allDigits = false
		default:
			return errors.New("name may only contain letters, digits and hyphens")
		}
	}
	if allDigits {
		return errors.New("name cannot be all digits")
	}
	return nil
}
