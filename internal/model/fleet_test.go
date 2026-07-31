package model

import (
	"math"
	"testing"
	"time"
)

func testFleet() *Fleet {
	return NewFleet([]HostRef{{Name: "nas", Addr: "nas"}})
}

func TestFirstSampleHasNoDerivedRates(t *testing.T) {
	f := testFleet()
	f.Apply("nas", Sample{
		At:     time.Unix(100, 0),
		HasCPU: true,
		CPU:    CPUTimes{User: 1000, Idle: 9000},
		NICs:   []NIC{{Name: "eth0", RxBytes: 500, TxBytes: 100}},
	})

	h, _ := f.Get("nas")
	if h.Status != StatusUp {
		t.Errorf("Status = %v, want up", h.Status)
	}
	// Rates need two points in time; reporting 0% here would be a convincing
	// lie rather than an absence.
	if h.HasCPUPct {
		t.Error("HasCPUPct = true after one sample, want false")
	}
	if h.HasNet {
		t.Error("HasNet = true after one sample, want false")
	}
	if h.CPUHist.Len() != 0 {
		t.Errorf("CPUHist.Len() = %d, want 0", h.CPUHist.Len())
	}
}

func TestCPUAndNetRatesFromTwoSamples(t *testing.T) {
	f := testFleet()
	f.Apply("nas", Sample{
		At:     time.Unix(100, 0),
		HasCPU: true,
		CPU:    CPUTimes{User: 1000, Idle: 9000},
		NICs:   []NIC{{Name: "eth0", RxBytes: 1000, TxBytes: 500}},
	})
	// 500 busy jiffies out of 1000 total elapsed = 50%.
	f.Apply("nas", Sample{
		At:     time.Unix(102, 0),
		HasCPU: true,
		CPU:    CPUTimes{User: 1500, Idle: 9500},
		NICs:   []NIC{{Name: "eth0", RxBytes: 3000, TxBytes: 1500}},
	})

	h, _ := f.Get("nas")
	if !h.HasCPUPct {
		t.Fatal("HasCPUPct = false, want true")
	}
	if got, want := h.CPUPct, 50.0; math.Abs(got-want) > 0.001 {
		t.Errorf("CPUPct = %v, want %v", got, want)
	}

	if !h.HasNet {
		t.Fatal("HasNet = false, want true")
	}
	rx, tx := h.TotalNet()
	// 2000 bytes over 2 seconds.
	if got, want := rx, 1000.0; math.Abs(got-want) > 0.001 {
		t.Errorf("rx = %v B/s, want %v", got, want)
	}
	if got, want := tx, 500.0; math.Abs(got-want) > 0.001 {
		t.Errorf("tx = %v B/s, want %v", got, want)
	}

	if h.CPUHist.Len() != 1 {
		t.Errorf("CPUHist.Len() = %d, want 1", h.CPUHist.Len())
	}
}

// TestCounterResetProducesNoRate covers the reboot case. Counters restart at
// zero, and diffing naively underflows into an enormous fictional spike.
func TestCounterResetProducesNoRate(t *testing.T) {
	f := testFleet()
	f.Apply("nas", Sample{
		At:     time.Unix(100, 0),
		HasCPU: true,
		CPU:    CPUTimes{User: 5_000_000, Idle: 90_000_000},
		NICs:   []NIC{{Name: "eth0", RxBytes: 9_000_000_000, TxBytes: 4_000_000_000}},
	})
	// Host rebooted: every counter is back near zero.
	f.Apply("nas", Sample{
		At:     time.Unix(102, 0),
		HasCPU: true,
		CPU:    CPUTimes{User: 120, Idle: 800},
		NICs:   []NIC{{Name: "eth0", RxBytes: 1024, TxBytes: 512}},
	})

	h, _ := f.Get("nas")
	if h.HasCPUPct {
		t.Errorf("HasCPUPct = true after counter reset (CPUPct = %v), want false", h.CPUPct)
	}
	if h.HasNet {
		rx, tx := h.TotalNet()
		t.Errorf("HasNet = true after counter reset (rx=%v tx=%v), want false", rx, tx)
	}
}

func TestDiskRatesFromTwoSamples(t *testing.T) {
	f := testFleet()
	f.Apply("nas", Sample{
		At:    time.Unix(100, 0),
		Disks: []Disk{{Name: "nvme0n1", SectorsRead: 1000, SectorsWritten: 2000}},
	})
	f.Apply("nas", Sample{
		At:    time.Unix(102, 0),
		Disks: []Disk{{Name: "nvme0n1", SectorsRead: 1400, SectorsWritten: 3000}},
	})

	h, _ := f.Get("nas")
	if !h.HasDisk {
		t.Fatal("HasDisk = false, want true")
	}
	read, write := h.TotalDisk()
	// 400 sectors × 512 bytes over 2 seconds.
	if got, want := read, 400.0*512/2; math.Abs(got-want) > 0.001 {
		t.Errorf("read = %v B/s, want %v", got, want)
	}
	if got, want := write, 1000.0*512/2; math.Abs(got-want) > 0.001 {
		t.Errorf("write = %v B/s, want %v", got, want)
	}
}

// TestDiskCounterResetProducesNoRate mirrors the network and CPU reboot guard:
// counters restarting must not underflow into an enormous fictional rate.
func TestDiskCounterResetProducesNoRate(t *testing.T) {
	f := testFleet()
	f.Apply("nas", Sample{
		At:    time.Unix(100, 0),
		Disks: []Disk{{Name: "nvme0n1", SectorsRead: 9_000_000, SectorsWritten: 8_000_000}},
	})
	f.Apply("nas", Sample{
		At:    time.Unix(102, 0),
		Disks: []Disk{{Name: "nvme0n1", SectorsRead: 12, SectorsWritten: 40}},
	})

	h, _ := f.Get("nas")
	if h.HasDisk {
		r, w := h.TotalDisk()
		t.Errorf("HasDisk = true after reset (r=%v w=%v), want false", r, w)
	}
}

func TestLoadPerCoreNeedsCoreCount(t *testing.T) {
	s := Sample{Load: [3]float64{8, 4, 2}, Cores: 16}
	ratio, ok := s.LoadPerCore()
	if !ok || math.Abs(ratio-0.5) > 0.0001 {
		t.Errorf("LoadPerCore() = %v, %v; want 0.5, true", ratio, ok)
	}

	// Without a core count the ratio is meaningless, so report nothing rather
	// than assuming a number.
	s.Cores = 0
	if _, ok := s.LoadPerCore(); ok {
		t.Error("LoadPerCore() ok = true with no core count, want false")
	}
}

func TestSwapPct(t *testing.T) {
	s := Sample{SwapTotal: 4000, SwapFree: 1000}
	if got, want := s.SwapPct(), 75.0; math.Abs(got-want) > 0.001 {
		t.Errorf("SwapPct() = %v, want %v", got, want)
	}
	// No swap configured must not divide by zero.
	if got := (Sample{}).SwapPct(); got != 0 {
		t.Errorf("SwapPct() with no swap = %v, want 0", got)
	}
}

func TestNewInterfaceIgnoredUntilItHasHistory(t *testing.T) {
	f := testFleet()
	f.Apply("nas", Sample{
		At:   time.Unix(100, 0),
		NICs: []NIC{{Name: "eth0", RxBytes: 1000}},
	})
	f.Apply("nas", Sample{
		At: time.Unix(102, 0),
		NICs: []NIC{
			{Name: "eth0", RxBytes: 3000},
			{Name: "wg0", RxBytes: 999999}, // appeared between polls
		},
	})

	h, _ := f.Get("nas")
	if got, want := len(h.NetRates), 1; got != want {
		t.Fatalf("NetRates = %d, want %d (new interface has nothing to diff)", got, want)
	}
	if h.NetRates[0].Name != "eth0" {
		t.Errorf("NetRates[0].Name = %q, want eth0", h.NetRates[0].Name)
	}
}

func TestFailureHysteresis(t *testing.T) {
	f := testFleet()
	f.Apply("nas", Sample{At: time.Unix(100, 0), HasMem: true, MemTotal: 100, MemAvail: 50})
	h, _ := f.Get("nas")

	// One miss is a blip: keep showing the last values rather than blanking a
	// row that is probably fine.
	f.Fail("nas", FailUnreachable, "timeout")
	if got, want := h.Status, StatusStale; got != want {
		t.Errorf("after 1 failure Status = %v, want %v", got, want)
	}
	if !h.Status.Live() {
		t.Error("stale host should still count as live")
	}

	f.Fail("nas", FailUnreachable, "timeout")
	if got, want := h.Status, StatusDown; got != want {
		t.Errorf("after 2 failures Status = %v, want %v", got, want)
	}
	if h.Status.Live() {
		t.Error("down host should not count as live")
	}

	// History survives an outage so you can see what it looked like before.
	if h.MemHist.Len() == 0 {
		t.Error("MemHist was cleared by failure, want preserved")
	}

	// Recovery resets the counter.
	f.Apply("nas", Sample{At: time.Unix(110, 0)})
	if got, want := h.Status, StatusUp; got != want {
		t.Errorf("after recovery Status = %v, want %v", got, want)
	}
}

func TestAuthFailureSkipsHysteresis(t *testing.T) {
	f := testFleet()
	// Rejected credentials are definitive: surfacing them immediately is the
	// point, since retrying can never clear the condition.
	f.Fail("nas", FailAuth, "permission denied")
	h, _ := f.Get("nas")
	if got, want := h.Status, StatusAuth; got != want {
		t.Errorf("Status = %v, want %v", got, want)
	}
}

func TestBadOutputSkipsHysteresis(t *testing.T) {
	f := testFleet()
	f.Fail("nas", FailBadOutput, "missing version line")
	h, _ := f.Get("nas")
	if got, want := h.Status, StatusBadOutput; got != want {
		t.Errorf("Status = %v, want %v", got, want)
	}
}

func TestRecoveryAfterFailureDoesNotDiffAcrossTheGap(t *testing.T) {
	f := testFleet()
	f.Apply("nas", Sample{
		At: time.Unix(100, 0), HasCPU: true,
		CPU: CPUTimes{User: 1000, Idle: 9000},
	})
	f.Fail("nas", FailUnreachable, "timeout")
	// The host may have rebooted while unreachable, so the first sample after
	// an outage must be treated as a fresh baseline.
	f.Apply("nas", Sample{
		At: time.Unix(200, 0), HasCPU: true,
		CPU: CPUTimes{User: 1100, Idle: 9100},
	})

	h, _ := f.Get("nas")
	if h.HasCPUPct {
		t.Errorf("HasCPUPct = true across an outage gap (%v), want false", h.CPUPct)
	}
}

func TestFilesystemFilter(t *testing.T) {
	f := NewFleet([]HostRef{{Name: "nas", Addr: "nas", Filesystems: []string{"/mnt/tank"}}})
	f.Apply("nas", Sample{
		At: time.Unix(100, 0),
		FS: []FS{
			{Mount: "/", TotalKB: 100, UsedKB: 50, AvailKB: 50},
			{Mount: "/mnt/tank", TotalKB: 999, UsedKB: 900, AvailKB: 99},
			{Mount: "/boot", TotalKB: 10, UsedKB: 1, AvailKB: 9},
		},
	})
	h, _ := f.Get("nas")
	if got, want := len(h.Cur.FS), 1; got != want {
		t.Fatalf("FS = %d, want %d", got, want)
	}
	if h.Cur.FS[0].Mount != "/mnt/tank" {
		t.Errorf("Mount = %q, want /mnt/tank", h.Cur.FS[0].Mount)
	}
}

func TestEmptyFilesystemFilterKeepsAll(t *testing.T) {
	// An unconfigured host must still show a filling disk.
	f := testFleet()
	f.Apply("nas", Sample{
		At: time.Unix(100, 0),
		FS: []FS{{Mount: "/"}, {Mount: "/mnt/tank"}},
	})
	h, _ := f.Get("nas")
	if got, want := len(h.Cur.FS), 2; got != want {
		t.Errorf("FS = %d, want %d", got, want)
	}
}

func TestRootFSPrefersSlashThenFullest(t *testing.T) {
	s := Sample{FS: []FS{
		{Mount: "/var", UsedKB: 90, AvailKB: 10},
		{Mount: "/", UsedKB: 10, AvailKB: 90},
	}}
	fs, ok := s.RootFS()
	if !ok || fs.Mount != "/" {
		t.Errorf("RootFS() = %q, want /", fs.Mount)
	}

	// With no root filesystem, the fullest one is the one worth showing.
	s = Sample{FS: []FS{
		{Mount: "/data", UsedKB: 10, AvailKB: 90},
		{Mount: "/mnt/tank", UsedKB: 90, AvailKB: 10},
	}}
	fs, ok = s.RootFS()
	if !ok || fs.Mount != "/mnt/tank" {
		t.Errorf("RootFS() = %q, want /mnt/tank", fs.Mount)
	}
}

func TestMemUsedUsesAvailableNotFree(t *testing.T) {
	s := Sample{HasMem: true, MemTotal: 1000, MemAvail: 250}
	if got, want := s.MemUsed(), uint64(750); got != want {
		t.Errorf("MemUsed() = %d, want %d", got, want)
	}
	if got, want := s.MemPct(), 75.0; math.Abs(got-want) > 0.001 {
		t.Errorf("MemPct() = %v, want %v", got, want)
	}
}

func TestRingWrapsAndPreservesOrder(t *testing.T) {
	var r Ring
	for i := 0; i < HistoryLen+5; i++ {
		r.Push(float64(i))
	}
	if got, want := r.Len(), HistoryLen; got != want {
		t.Fatalf("Len() = %d, want %d (capacity must be fixed)", got, want)
	}
	vals := r.Values()
	// Oldest first, and the earliest five must have been discarded.
	if got, want := vals[0], 5.0; got != want {
		t.Errorf("oldest = %v, want %v", got, want)
	}
	if got, want := vals[len(vals)-1], float64(HistoryLen+4); got != want {
		t.Errorf("newest = %v, want %v", got, want)
	}
	last, ok := r.Last()
	if !ok || last != float64(HistoryLen+4) {
		t.Errorf("Last() = %v, %v", last, ok)
	}
}

// TestContainerWatchListFlagsMissing covers the case the watch list exists
// for: a container that should be there and simply is not. Silently omitting
// it would leave the host looking healthy.
func TestContainerWatchListFlagsMissing(t *testing.T) {
	f := NewFleet([]HostRef{{
		Name: "nas", Addr: "nas",
		Containers: []string{"web", "db", "gone"},
	}})
	f.Apply("nas", Sample{
		At: time.Unix(100, 0),
		Containers: []Container{
			{Runtime: "docker", Name: "web", State: "running"},
			{Runtime: "docker", Name: "db", State: "exited"},
			{Runtime: "docker", Name: "unwatched", State: "running"},
		},
	})

	h, _ := f.Get("nas")
	if got, want := len(h.Cur.Containers), 3; got != want {
		t.Fatalf("containers = %d, want %d (watched only)", got, want)
	}

	byName := map[string]Container{}
	for _, c := range h.Cur.Containers {
		byName[c.Name] = c
	}
	if !byName["web"].Running() {
		t.Error("web should be running")
	}
	if byName["gone"].State != ContainerMissing {
		t.Errorf("gone state = %q, want %q", byName["gone"].State, ContainerMissing)
	}
	if _, ok := byName["unwatched"]; ok {
		t.Error("unwatched container survived the filter")
	}

	stopped := h.Cur.StoppedContainers()
	if got, want := len(stopped), 2; got != want {
		t.Errorf("stopped = %d, want %d (db exited, gone missing)", got, want)
	}
}

func TestEmptyContainerWatchListKeepsAll(t *testing.T) {
	f := testFleet()
	f.Apply("nas", Sample{
		At: time.Unix(100, 0),
		Containers: []Container{
			{Name: "a", State: "running"}, {Name: "b", State: "exited"},
		},
	})
	h, _ := f.Get("nas")
	if got, want := len(h.Cur.Containers), 2; got != want {
		t.Errorf("containers = %d, want %d", got, want)
	}
}

// TestStoppedServicesIgnoresMissingUnits keeps a configuration mismatch from
// reading as an outage: watching "caddy" on a host where caddy runs in a
// container should not raise a permanent alarm.
func TestStoppedServicesIgnoresMissingUnits(t *testing.T) {
	s := Sample{Services: []Service{
		{Name: "ssh", LoadState: "loaded", ActiveState: "active"},
		{Name: "postgresql", LoadState: "loaded", ActiveState: "inactive"},
		{Name: "caddy", LoadState: "not-found", ActiveState: "inactive"},
	}}
	stopped := s.StoppedServices()
	if got, want := len(stopped), 1; got != want {
		t.Fatalf("stopped = %d, want %d: %+v", got, want, stopped)
	}
	if stopped[0].Name != "postgresql" {
		t.Errorf("stopped = %q, want postgresql", stopped[0].Name)
	}
}

func TestJustRebooted(t *testing.T) {
	if !(Sample{Uptime: 2 * time.Minute}).JustRebooted() {
		t.Error("2m uptime should count as just rebooted")
	}
	if (Sample{Uptime: 26 * 24 * time.Hour}).JustRebooted() {
		t.Error("26d uptime should not count as just rebooted")
	}
	// Zero uptime means the host never reported one, not that it just booted.
	if (Sample{}).JustRebooted() {
		t.Error("absent uptime should not count as just rebooted")
	}
}
