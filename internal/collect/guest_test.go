package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sousf/hmon/internal/model"
)

// guestOutput wraps guest lines in the minimum a host reading needs to parse.
func guestOutput(lines ...string) []byte {
	all := append([]string{"v 1"}, lines...)
	return []byte(strings.Join(append(all, "end", ""), "\n"))
}

func TestParseGuestListing(t *testing.T) {
	s, err := Parse(guestOutput(
		"guestsreported 1",
		"guest nixos vm running 256.00MiB 6.5% 11s 13",
		"guest web ct stopped - - - -",
	), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasGuestInfo {
		t.Error("HasGuestInfo = false, want true")
	}
	if len(s.Guests) != 2 {
		t.Fatalf("got %d guests, want 2", len(s.Guests))
	}

	vm := s.Guests[0]
	if vm.Name != "nixos" || vm.Kind != model.GuestVM || vm.State != "running" {
		t.Errorf("vm = %+v", vm)
	}
	if want := uint64(256 * 1024 * 1024); vm.MemUsed != want {
		t.Errorf("MemUsed = %d, want %d", vm.MemUsed, want)
	}
	if vm.MemPct != 6.5 || vm.CPUSecs != 11 || vm.Procs != 13 {
		t.Errorf("accounting = %v %v %v, want 6.5 11 13", vm.MemPct, vm.CPUSecs, vm.Procs)
	}
	if vm.Probed {
		t.Error("Probed = true with no sub-document")
	}

	ct := s.Guests[1]
	if ct.Kind != model.GuestContainer || ct.Running() {
		t.Errorf("ct = %+v, want a stopped container", ct)
	}
	// "-" stands in for a column LXD left blank, and must not become a zero
	// that looks like a real measurement.
	if ct.MemUsed != 0 || ct.MemPct != 0 || ct.CPUSecs != 0 {
		t.Errorf("blank columns produced values: %+v", ct)
	}
}

// A guest's own reading arrives as a sub-document, and is the whole point of
// the feature: the numbers come from inside the instance, not from the daemon.
func TestParseGuestSubDocument(t *testing.T) {
	s, err := Parse(guestOutput(
		"guestsreported 1",
		"guest nixos vm running 256.00MiB 6.5% 11s 13",
		"g nixos v 1",
		"g nixos uptime 1429.96",
		"g nixos cpu 165 0 494 570277 122 342 174 104",
		"g nixos mem 3987056 3641704",
		"g nixos cores 4",
		"g nixos fs / 9983492 1809844 7713864",
		"g nixos net enp5s0 3190 4380",
		"g nixos end",
	), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	g := s.Guests[0]
	if !g.Probed {
		t.Fatal("Probed = false, want true")
	}
	if !g.Sample.HasCPU || g.Sample.CPU.User != 165 {
		t.Errorf("CPU = %+v", g.Sample.CPU)
	}
	if g.Sample.MemTotal != 3987056*1024 {
		t.Errorf("MemTotal = %d", g.Sample.MemTotal)
	}
	if g.Sample.Cores != 4 || g.Sample.Uptime != 1429960*time.Millisecond {
		t.Errorf("cores/uptime = %d %s", g.Sample.Cores, g.Sample.Uptime)
	}
	if len(g.Sample.FS) != 1 || len(g.Sample.NICs) != 1 {
		t.Errorf("fs=%d nics=%d, want 1 and 1", len(g.Sample.FS), len(g.Sample.NICs))
	}
}

// A probe cut short mid-write is discarded whole. Applying half of it would
// render the guest as a machine that had just lost most of its filesystems.
func TestTruncatedGuestProbeIsDiscarded(t *testing.T) {
	s, err := Parse(guestOutput(
		"guestsreported 1",
		"guest nixos vm running 256.00MiB 6.5% 11s 13",
		"g nixos v 1",
		"g nixos mem 3987056 3641704",
		// no "g nixos end"
	), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Guests[0].Probed {
		t.Error("Probed = true for a truncated sub-document")
	}
	// The listing still stands: the daemon's own accounting was never in doubt.
	if s.Guests[0].State != "running" {
		t.Errorf("State = %q, want running", s.Guests[0].State)
	}
}

// The host's own reading has to survive a guest whose output is nonsense,
// because one wedged instance should not blank out the machine running it.
func TestGuestFailureLeavesHostSampleIntact(t *testing.T) {
	s, err := Parse(guestOutput(
		"mem 16000000 8000000",
		"guestsreported 1",
		"guest nixos vm running - - - -",
		"g nixos v 99",
		"g nixos mem garbage",
	), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasMem || s.MemTotal != 16000000*1024 {
		t.Errorf("host memory lost: HasMem=%v total=%d", s.HasMem, s.MemTotal)
	}
	if s.Guests[0].Probed {
		t.Error("a sub-document with the wrong version was accepted")
	}
}

func TestGuestCoresRecorded(t *testing.T) {
	s, err := Parse(guestOutput(
		"guestsreported 1",
		"guest nixos vm running 1.00GiB 25% 40s 13",
		"guestcores nixos 4",
	), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Guests[0].Cores != 4 {
		t.Errorf("Cores = %d, want 4", s.Guests[0].Cores)
	}
}

// Without the sentinel a poll that skipped the section is indistinguishable
// from one where every instance was destroyed.
func TestGuestSectionNotRun(t *testing.T) {
	s, err := Parse(guestOutput("mem 16000000 8000000"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.HasGuestInfo {
		t.Error("HasGuestInfo = true when the section never ran")
	}
	if len(s.Guests) != 0 {
		t.Errorf("got %d guests, want none", len(s.Guests))
	}
}

// The fixture is real output from the collector script, captured in a Debian
// container against a stub lxc that covers all three outcomes at once: an
// instance the probe reached, one it could not, and one that is stopped.
func TestParseRealGuestCollectorOutput(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "debian_guests.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasGuestInfo || len(s.Guests) != 3 {
		t.Fatalf("guests = %d (reported %v), want 3", len(s.Guests), s.HasGuestInfo)
	}

	byName := map[string]model.Guest{}
	for _, g := range s.Guests {
		byName[g.Name] = g
	}

	good := byName["good"]
	if !good.Probed || good.Kind != model.GuestContainer {
		t.Errorf("good = %+v, want a probed container", good)
	}
	if !good.Sample.HasMem || good.Sample.MemTotal != 2048000*1024 {
		t.Errorf("good memory = %d", good.Sample.MemTotal)
	}

	bad := byName["bad"]
	if bad.Probed {
		t.Error("bad was marked probed despite lxc exec failing")
	}
	if bad.Cores != 2 || bad.CPUSecs != 20 {
		t.Errorf("bad fallback = cores %d, cpu %v; want 2 and 20", bad.Cores, bad.CPUSecs)
	}

	if off := byName["off"]; off.Running() || off.Probed {
		t.Errorf("off = %+v, want stopped and unprobed", off)
	}

	// The host's own reading has to come through intact alongside all of that.
	// (No systemd in the container, so unit state is legitimately absent.)
	if s.Cores == 0 || len(s.FS) == 0 || !s.HasCPU || !s.HasMem {
		t.Errorf("host reading degraded: cores=%d fs=%d cpu=%v mem=%v",
			s.Cores, len(s.FS), s.HasCPU, s.HasMem)
	}
	// A guest reports no temperatures: inside a container /sys still shows the
	// host's sensors, so the section is suppressed rather than duplicated.
	for _, g := range s.Guests {
		if len(g.Sample.Temps) > 0 {
			t.Errorf("%s reported temperatures", g.Name)
		}
	}
}

func TestParseIECBytes(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"512B", 512, true},
		{"12.00kiB", 12 * 1024, true},
		{"256.00MiB", 256 << 20, true},
		{"1.50GiB", 1536 << 20, true},
		{"2.00TiB", 2 << 40, true},
		// Case is not guaranteed across LXD versions, and neither is a suffix.
		{"8MIB", 8 << 20, true},
		{"1024", 1024, true},
		{"-", 0, false},
		{"", 0, false},
		{"MiB", 0, false},
		{"12XiB", 0, false},
	}
	for _, c := range cases {
		got, ok := parseIECBytes(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseIECBytes(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseSecondsAcceptsDurations(t *testing.T) {
	// LXD renders CPU usage as a duration, so an instance that has been busy
	// for a while reports "3h25m45s" rather than a plain number of seconds.
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"11s", 11, true},
		{"3h25m45s", 3*3600 + 25*60 + 45, true},
		{"0s", 0, true},
		{"-", 0, false},
		{"", 0, false},
		{"nonsense", 0, false},
	}
	for _, c := range cases {
		got, ok := parseSeconds(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseSeconds(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParsePercent(t *testing.T) {
	if v, ok := parsePercent("6.5%"); !ok || v != 6.5 {
		t.Errorf("parsePercent(6.5%%) = %v, %v", v, ok)
	}
	if _, ok := parsePercent("-"); ok {
		t.Error("parsePercent(-) accepted a blank column")
	}
}

// The guest probe is the collector itself, quoted into a variable. If the
// quoting were wrong the remote shell would try to execute it.
func TestRemoteInputCarriesGuestProbe(t *testing.T) {
	plain := remoteInput([]string{"procs"})
	if plain != collectorScript {
		t.Error("a poll without guests should send the collector alone")
	}

	withGuests := remoteInput([]string{"guests"})
	if !strings.HasPrefix(withGuests, "HMON_GUEST_PROBE='") {
		t.Fatalf("missing probe assignment, got %.40q", withGuests)
	}
	if !strings.HasSuffix(withGuests, collectorScript) {
		t.Error("the collector itself should still follow the assignment")
	}

	// Every single quote in the payload must be escaped, or the assignment
	// closes early and the rest of the script runs as commands.
	assignment := strings.TrimSuffix(withGuests, "\n"+collectorScript)
	body := strings.TrimSuffix(strings.TrimPrefix(assignment, "HMON_GUEST_PROBE='"), "'")
	if strings.Contains(strings.ReplaceAll(body, `'\''`, ""), "'") {
		t.Error("an unescaped single quote survived quoting")
	}
}

func TestShellQuoteRoundTrips(t *testing.T) {
	for _, in := range []string{"", "plain", "it's", `a'b'c`, "multi\nline", `$(rm -rf /)`} {
		got := shellQuote(in)
		want := "'" + strings.ReplaceAll(in, "'", `'\''`) + "'"
		if got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOptsRequestGuests(t *testing.T) {
	args := Opts{Guests: true, GuestProcs: "nixos"}.args()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "guests") || !strings.Contains(joined, "gprocs=nixos") {
		t.Errorf("args = %v", args)
	}

	// Processes for a guest are only ever asked for the row on screen.
	if joined := strings.Join(Opts{Guests: true}.args(), " "); strings.Contains(joined, "gprocs") {
		t.Errorf("args = %q, want no gprocs", joined)
	}
	if joined := strings.Join(Opts{}.args(), " "); strings.Contains(joined, "guests") {
		t.Errorf("args = %q, want no guests", joined)
	}
}
