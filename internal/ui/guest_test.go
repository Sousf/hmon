package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/Sousf/hmon/internal/model"
)

// seedGuests gives a host the instances named, all running and probed, and
// returns the model with the fleet updated.
func seedGuests(t *testing.T, m Model, f *model.Fleet, host string, names ...string) Model {
	t.Helper()
	at := time.Now()
	guests := make([]model.Guest, 0, len(names))
	for i, n := range names {
		guests = append(guests, model.Guest{
			Name: n, Kind: model.GuestVM, State: "running", Probed: true,
			Sample: model.Sample{
				At: at, HasMem: true, MemTotal: 4 << 30, MemAvail: uint64(i+1) << 30,
			},
		})
	}
	f.Apply(host, model.Sample{At: at, HasGuestInfo: true, Guests: guests, Cores: 8})
	return m
}

func rowNames(m Model) []string {
	hosts := m.sortedHosts()
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.Name
	}
	return out
}

// Guests stay under the machine running them whatever the table is sorted by.
// Letting them join the global ordering would break the tree apart exactly when
// you most want to see a VM next to its host: when it gets busy.
func TestGuestsStayUnderTheirHostAcrossSorts(t *testing.T) {
	m, f := testModel(t, "alpha", "beta")
	m = seedGuests(t, m, f, "beta", "two", "one")

	want := []string{"alpha", "beta", "beta/one", "beta/two"}
	if got := rowNames(m); !equal(got, want) {
		t.Errorf("name sort = %v, want %v", got, want)
	}

	// Whichever column the table is ordered by, and in whichever direction, a
	// guest must stay immediately beneath its own machine.
	for _, key := range []sortKey{sortName, sortStatus, sortCPU, sortMem, sortDisk, sortTemp} {
		for _, desc := range []bool{false, true} {
			m.sort, m.sortDesc = key, desc
			got := rowNames(m)
			if err := checkTree(got); err != "" {
				t.Errorf("sort=%v desc=%v: %s (order %v)", key, desc, err, got)
			}
		}
	}
}

// checkTree reports the first way an ordering fails to read as a tree: a guest
// that does not sit under its own machine.
func checkTree(rows []string) string {
	parent := ""
	for _, name := range rows {
		host, _, isGuest := strings.Cut(name, "/")
		if !isGuest {
			parent = name
			continue
		}
		if host != parent {
			return "guest " + name + " is not beneath " + host
		}
	}
	return ""
}

func TestTreePrefixesMarkTheLastGuest(t *testing.T) {
	m, f := testModel(t, "alpha", "beta")
	m = seedGuests(t, m, f, "alpha", "one", "two")

	prefixes := treePrefixes(m.sortedHosts())
	want := []string{"", "├─ ", "└─ ", ""}
	if !equal(prefixes, want) {
		t.Errorf("prefixes = %q, want %q", prefixes, want)
	}
}

// A guest has no address; its readings arrive on its host's poll.
func TestGuestsAreNotPolledDirectly(t *testing.T) {
	m, f := testModel(t, "alpha", "beta")
	m = seedGuests(t, m, f, "beta", "one", "two")

	poller := m.poller.(*fakePoller)
	drain(m.pollAll())
	if got := len(poller.opts); got != 2 {
		t.Errorf("issued %d polls, want 2 — one per machine", got)
	}
}

// Processes cost a sampling window inside the guest too, so only the row on
// screen pays for them — and they are asked of the host, which is the only
// thing hmon can connect to.
func TestSelectedGuestRequestsItsOwnProcesses(t *testing.T) {
	m, f := testModel(t, "alpha")
	m = seedGuests(t, m, f, "alpha", "nixos")

	m.selected = "alpha/nixos"
	m.view = viewDetail

	poller := m.poller.(*fakePoller)
	drain(m.pollAll())
	if len(poller.opts) == 0 {
		t.Fatal("no poll was issued")
	}
	last := poller.opts[len(poller.opts)-1]
	if last.GuestProcs != "nixos" {
		t.Errorf("GuestProcs = %q, want nixos", last.GuestProcs)
	}
	// The host's own process list is not what is on screen, so it is not worth
	// the extra half-second.
	if last.Detail {
		t.Error("Detail = true; the host's processes were not asked for")
	}
}

func TestSelectedHostStillRequestsItsOwnProcesses(t *testing.T) {
	m, f := testModel(t, "alpha")
	m = seedGuests(t, m, f, "alpha", "nixos")

	m.selected = "alpha"
	m.view = viewDetail

	poller := m.poller.(*fakePoller)
	drain(m.pollAll())
	last := poller.opts[len(poller.opts)-1]
	if !last.Detail || last.GuestProcs != "" {
		t.Errorf("Detail=%v GuestProcs=%q, want true and empty", last.Detail, last.GuestProcs)
	}
}

// Everything hmon does beyond looking goes over ssh to an address, and a guest
// has none. Rebooting one would mean `lxc restart` on its host — a different
// command with different consequences, not something to hide behind the same
// key.
func TestActionsRefuseGuests(t *testing.T) {
	m, f := testModel(t, "alpha")
	m = seedGuests(t, m, f, "alpha", "nixos")
	m.selected = "alpha/nixos"

	if got := send(m, key("R")); got.confirmReboot != "" {
		t.Errorf("reboot dialog opened for a guest: %q", got.confirmReboot)
	}
	if got := send(m, key(" ")); len(got.marked) != 0 {
		t.Errorf("a guest was marked: %v", got.marked)
	}
	if got := send(m, key("x")); got.view == viewPrompt {
		t.Error("the command prompt opened with a guest selected")
	}
	if got := m.targets(); len(got) != 0 {
		t.Errorf("targets = %v, want none", got)
	}

	// Marking everything picks up machines only, so a later fan-out cannot
	// reach a guest by accident.
	got := send(m, key("a"))
	if len(got.marked) != 1 || !got.marked["alpha"] {
		t.Errorf("marked = %v, want just the machine", got.marked)
	}
}

func TestActionsStillWorkOnMachines(t *testing.T) {
	m, f := testModel(t, "alpha")
	m = seedGuests(t, m, f, "alpha", "nixos")
	m.selected = "alpha"

	if got := send(m, key("R")); got.confirmReboot != "alpha" {
		t.Errorf("confirmReboot = %q, want alpha", got.confirmReboot)
	}
	if got := send(m, key(" ")); !got.marked["alpha"] {
		t.Error("the machine could not be marked")
	}
}

// The cursor walks the flattened tree, so a guest is reachable with the arrow
// keys and openable with enter.
func TestCursorReachesGuests(t *testing.T) {
	m, f := testModel(t, "alpha", "beta")
	m = seedGuests(t, m, f, "alpha", "nixos")

	m = send(m, key("down"))
	if m.selected != "alpha/nixos" {
		t.Fatalf("selected = %q, want alpha/nixos", m.selected)
	}
	m = send(m, key("down"))
	if m.selected != "beta" {
		t.Errorf("selected = %q, want beta", m.selected)
	}
}

func TestGuestRowRendersAsATreeBranch(t *testing.T) {
	m, f := testModel(t, "alpha")
	m = seedGuests(t, m, f, "alpha", "nixos")
	m.width, m.height = 120, 12

	out := m.renderTable()
	line := lineContaining(t, out, "nixos")
	if !strings.Contains(line, "└─ nixos") {
		t.Errorf("no branch drawn: %q", line)
	}
	if !strings.Contains(line, "vm") {
		t.Errorf("no kind tag: %q", line)
	}
}

// A stopped instance is not a failure, and must not drag the fleet summary
// down as though something had gone wrong.
func TestStoppedGuestIsExcludedFromTheUpCount(t *testing.T) {
	m, f := testModel(t, "alpha")
	at := time.Now()
	f.Apply("alpha", model.Sample{
		At: at, HasGuestInfo: true,
		Guests: []model.Guest{{Name: "nixos", Kind: model.GuestVM, State: "stopped"}},
	})
	m.width, m.height = 120, 12

	out := m.renderTable()
	if !strings.Contains(out, "1/1 up") {
		t.Errorf("summary counted a stopped guest: %q", firstLine(out))
	}
	if line := lineContaining(t, out, "nixos"); !strings.Contains(line, "stopped") {
		t.Errorf("row = %q, want it labelled stopped", line)
	}
}

func lineContaining(t *testing.T, s, want string) string {
	t.Helper()
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", want, s)
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// i inverts the sort, and has to do so for the name column too — the one
// selected by default, and so the one most likely to be inverted first.
func TestInvertReversesNameSort(t *testing.T) {
	m, _ := testModel(t, "alpha", "beta", "gamma")

	m.sort, m.sortDesc = sortName, false
	if got := rowNames(m); !equal(got, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("ascending = %v", got)
	}
	m.sortDesc = true
	if got := rowNames(m); !equal(got, []string{"gamma", "beta", "alpha"}) {
		t.Errorf("descending = %v, want the reverse", got)
	}
}

// The control: a metric column reverses too, and equal values still tie-break
// by name ascending in both directions so rows do not reshuffle between frames.
func TestInvertReversesMetricSortButNotTies(t *testing.T) {
	m, f := testModel(t, "alpha", "beta", "gamma")
	at := time.Now()
	for name, avail := range map[string]uint64{
		"alpha": 1 << 30, "beta": 2 << 30, "gamma": 3 << 30,
	} {
		f.Apply(name, model.Sample{At: at, HasMem: true, MemTotal: 4 << 30, MemAvail: avail})
	}

	// alpha uses the most memory, gamma the least.
	m.sort, m.sortDesc = sortMem, true
	if got := rowNames(m); !equal(got, []string{"alpha", "beta", "gamma"}) {
		t.Errorf("descending = %v, want fullest first", got)
	}
	m.sortDesc = false
	if got := rowNames(m); !equal(got, []string{"gamma", "beta", "alpha"}) {
		t.Errorf("ascending = %v, want emptiest first", got)
	}

	// With nothing polled every value ties, and the name tie-break holds
	// whichever way the sort is pointing.
	m2, _ := testModel(t, "alpha", "beta", "gamma")
	m2.sort = sortCPU
	for _, desc := range []bool{false, true} {
		m2.sortDesc = desc
		if got := rowNames(m2); !equal(got, []string{"alpha", "beta", "gamma"}) {
			t.Errorf("desc=%v: ties ordered %v, want name ascending", desc, got)
		}
	}
}
