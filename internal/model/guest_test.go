package model

import (
	"testing"
	"time"
)

func guestFleet() *Fleet {
	return NewFleet([]HostRef{{Name: "hermes3", Addr: "hermes3"}})
}

// hostSample is a minimal host reading that also carries guests.
func hostSample(at time.Time, guests ...Guest) Sample {
	return Sample{At: at, HasGuestInfo: true, Guests: guests, Cores: 16}
}

func guestRow(t *testing.T, f *Fleet, name string) *Host {
	t.Helper()
	h, ok := f.Get(GuestKey("hermes3", name))
	if !ok {
		t.Fatalf("no row for guest %q", name)
	}
	return h
}

func TestGuestBecomesAHostUnderItsParent(t *testing.T) {
	f := guestFleet()
	at := time.Now()
	f.Apply("hermes3", hostSample(at, Guest{
		Name: "nixos", Kind: GuestVM, State: "running", Probed: true,
		Sample: Sample{At: at, HasMem: true, MemTotal: 4 << 30, MemAvail: 3 << 30},
	}))

	g := guestRow(t, f, "nixos")
	if g.Parent != "hermes3" || g.Kind != KindVM {
		t.Errorf("parent/kind = %q %v", g.Parent, g.Kind)
	}
	if !g.IsGuest() || g.Display() != "nixos" {
		t.Errorf("IsGuest=%v Display=%q", g.IsGuest(), g.Display())
	}
	if g.Status != StatusUp || !g.Cur.HasMem {
		t.Errorf("status=%v hasMem=%v", g.Status, g.Cur.HasMem)
	}

	// Order matters: a guest sits directly after the machine running it, so the
	// flat slice already reads as a tree.
	if len(f.Hosts) != 2 || f.Hosts[0].Name != "hermes3" || f.Hosts[1] != g {
		t.Errorf("Hosts order = %v", names(f.Hosts))
	}
}

// The whole reason a guest is a Host: every derived value written for machines
// works on it untouched.
func TestGuestGetsCPUAndRatesFromItsOwnCounters(t *testing.T) {
	f := guestFleet()
	t0 := time.Now()

	probe := func(at time.Time, busy, idle uint64, rx uint64) Guest {
		return Guest{
			Name: "nixos", Kind: GuestVM, State: "running", Probed: true,
			Sample: Sample{
				At: at, HasCPU: true,
				CPU:  CPUTimes{User: busy, Idle: idle},
				NICs: []NIC{{Name: "enp5s0", RxBytes: rx, TxBytes: 0}},
			},
		}
	}

	f.Apply("hermes3", hostSample(t0, probe(t0, 100, 900, 1000)))
	if g := guestRow(t, f, "nixos"); g.HasCPUPct {
		t.Error("a first sample cannot yield a rate")
	}

	t1 := t0.Add(10 * time.Second)
	f.Apply("hermes3", hostSample(t1, probe(t1, 300, 1700, 11000)))

	g := guestRow(t, f, "nixos")
	// 200 busy jiffies of 1000 elapsed.
	if !g.HasCPUPct || g.CPUPct != 20 {
		t.Errorf("CPUPct = %v (%v), want 20", g.CPUPct, g.HasCPUPct)
	}
	rx, _ := g.TotalNet()
	if !g.HasNet || rx != 1000 {
		t.Errorf("rx = %v (%v), want 1000 B/s", rx, g.HasNet)
	}
	if len(g.CPUHist.Values()) != 1 {
		t.Errorf("trend history not recorded: %v", g.CPUHist.Values())
	}
}

// A running instance we could not get inside still has LXD's own accounting.
func TestUnprobedGuestUsesDaemonAccounting(t *testing.T) {
	f := guestFleet()
	t0 := time.Now()

	thin := func(cpuSecs float64) Guest {
		return Guest{
			Name: "novm", Kind: GuestVM, State: "running",
			MemUsed: 512 << 20, MemPct: 25, CPUSecs: cpuSecs, Cores: 2,
		}
	}

	f.Apply("hermes3", hostSample(t0, thin(10)))
	g := guestRow(t, f, "novm")
	if g.Status != StatusUp {
		t.Errorf("status = %v, want up", g.Status)
	}
	// Total is recovered from usage and its percentage: 512MiB at 25% is 2GiB.
	if !g.Cur.HasMem || g.Cur.MemTotal != 2<<30 || g.Cur.MemUsed() != 512<<20 {
		t.Errorf("mem = %d of %d", g.Cur.MemUsed(), g.Cur.MemTotal)
	}
	if g.HasCPUPct {
		t.Error("a cumulative counter cannot yield a rate on its first reading")
	}

	// One second of CPU over ten wall-clock seconds across two cores is 5%.
	t1 := t0.Add(10 * time.Second)
	f.Apply("hermes3", hostSample(t1, thin(11)))
	g = guestRow(t, f, "novm")
	if !g.HasCPUPct || g.CPUPct != 5 {
		t.Errorf("CPUPct = %v (%v), want 5", g.CPUPct, g.HasCPUPct)
	}
}

// An instance with no CPU limit may use every core its host has, which makes
// the host's count the right divisor.
func TestUnlimitedGuestDividesByHostCores(t *testing.T) {
	f := guestFleet()
	t0 := time.Now()
	thin := func(cpuSecs float64) Guest {
		return Guest{Name: "novm", State: "running", CPUSecs: cpuSecs, Cores: 0}
	}

	f.Apply("hermes3", hostSample(t0, thin(0)))
	t1 := t0.Add(10 * time.Second)
	f.Apply("hermes3", hostSample(t1, thin(16)))

	// 16 CPU-seconds over 10 seconds across the host's 16 cores is 10%.
	if g := guestRow(t, f, "novm"); !g.HasCPUPct || g.CPUPct != 10 {
		t.Errorf("CPUPct = %v (%v), want 10", g.CPUPct, g.HasCPUPct)
	}
}

// The counter restarts when the instance does, and diffing across that
// boundary would produce a negative rate.
func TestGuestCPUCounterResetProducesNoRate(t *testing.T) {
	f := guestFleet()
	t0 := time.Now()
	thin := func(cpuSecs float64) Guest {
		return Guest{Name: "novm", State: "running", CPUSecs: cpuSecs, Cores: 2}
	}

	f.Apply("hermes3", hostSample(t0, thin(500)))
	f.Apply("hermes3", hostSample(t0.Add(10*time.Second), thin(600)))
	if g := guestRow(t, f, "novm"); !g.HasCPUPct {
		t.Fatal("expected a rate before the reset")
	}
	f.Apply("hermes3", hostSample(t0.Add(20*time.Second), thin(3)))
	if g := guestRow(t, f, "novm"); g.HasCPUPct {
		t.Error("a restarted counter produced a rate")
	}
}

func TestStoppedGuestIsNotAFailure(t *testing.T) {
	f := guestFleet()
	t0 := time.Now()

	f.Apply("hermes3", hostSample(t0, Guest{
		Name: "nixos", Kind: GuestVM, State: "running", Probed: true,
		Sample: Sample{At: t0, HasMem: true, MemTotal: 4 << 30, MemAvail: 1 << 30},
	}))
	f.Apply("hermes3", hostSample(t0.Add(time.Second), Guest{
		Name: "nixos", Kind: GuestVM, State: "stopped",
	}))

	g := guestRow(t, f, "nixos")
	if g.Status != StatusStopped {
		t.Errorf("status = %v, want stopped", g.Status)
	}
	if g.Status.Live() {
		t.Error("a stopped instance has no live readings")
	}
	// Readings from before it was shut down would imply a machine still running.
	if g.Cur.HasMem {
		t.Error("stale memory survived the stop")
	}
}

func TestGuestsAppearAndDisappearWithTheListing(t *testing.T) {
	f := guestFleet()
	t0 := time.Now()

	f.Apply("hermes3", hostSample(t0,
		Guest{Name: "a", State: "running"},
		Guest{Name: "b", State: "running"},
	))
	if len(f.Hosts) != 3 {
		t.Fatalf("Hosts = %v, want the machine and two guests", names(f.Hosts))
	}

	f.Apply("hermes3", hostSample(t0.Add(time.Second), Guest{Name: "b", State: "running"}))
	if len(f.Hosts) != 2 {
		t.Fatalf("Hosts = %v, want the deleted instance gone", names(f.Hosts))
	}
	if _, ok := f.Get(GuestKey("hermes3", "a")); ok {
		t.Error("a deleted instance is still in the index")
	}
	if _, ok := f.Get(GuestKey("hermes3", "b")); !ok {
		t.Error("the surviving instance was dropped")
	}
}

// The lesson containers already taught: a poll that skipped the section looks
// exactly like one where every instance was destroyed.
func TestUncollectedGuestsAreNotDropped(t *testing.T) {
	f := guestFleet()
	t0 := time.Now()

	f.Apply("hermes3", hostSample(t0, Guest{Name: "nixos", State: "running"}))
	// A poll with no guest information at all — the section never ran.
	f.Apply("hermes3", Sample{At: t0.Add(time.Second)})

	if _, ok := f.Get(GuestKey("hermes3", "nixos")); !ok {
		t.Error("the guest was dropped by a poll that never asked about guests")
	}
}

// A guest is only ever visible through its host, so an unreachable host takes
// its instances with it rather than leaving them reading "up".
func TestUnreachableHostTakesItsGuestsDown(t *testing.T) {
	f := guestFleet()
	t0 := time.Now()

	f.Apply("hermes3", hostSample(t0, Guest{
		Name: "nixos", State: "running", Probed: true,
		Sample: Sample{At: t0, HasCPU: true, CPU: CPUTimes{User: 1, Idle: 9}},
	}))
	f.Apply("hermes3", hostSample(t0.Add(time.Second), Guest{
		Name: "nixos", State: "running", Probed: true,
		Sample: Sample{At: t0.Add(time.Second), HasCPU: true, CPU: CPUTimes{User: 2, Idle: 18}},
	}))

	for i := 0; i < DownAfterFailures; i++ {
		f.Fail("hermes3", FailUnreachable, "no route to host")
	}

	g := guestRow(t, f, "nixos")
	if g.Status != StatusDown {
		t.Errorf("guest status = %v, want down", g.Status)
	}
	if g.HasCPUPct {
		t.Error("rates survived the host going away")
	}
}

// Two machines may each run an instance of the same name.
func TestGuestNamesAreScopedToTheirHost(t *testing.T) {
	f := NewFleet([]HostRef{
		{Name: "hermes2", Addr: "hermes2"},
		{Name: "hermes3", Addr: "hermes3"},
	})
	at := time.Now()
	f.Apply("hermes2", hostSample(at, Guest{Name: "nixos", State: "running"}))
	f.Apply("hermes3", hostSample(at, Guest{Name: "nixos", State: "stopped"}))

	a, okA := f.Get(GuestKey("hermes2", "nixos"))
	b, okB := f.Get(GuestKey("hermes3", "nixos"))
	if !okA || !okB || a == b {
		t.Fatal("same-named instances on different hosts collided")
	}
	if a.Status == b.Status {
		t.Errorf("both resolved to the same row: %v", a.Status)
	}

	// Each stays beneath its own machine.
	if got := names(f.Hosts); got[0] != "hermes2" || got[1] != "hermes2/nixos" ||
		got[2] != "hermes3" || got[3] != "hermes3/nixos" {
		t.Errorf("Hosts order = %v", got)
	}
}

func names(hosts []*Host) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.Name
	}
	return out
}
