package model

import "time"

// DownAfterFailures is how many consecutive failed polls flip a host from
// "stale" to "down". A single dropped packet should not make the whole table
// blink, but a genuinely dead machine should not take half a minute to show up
// either.
const DownAfterFailures = 2

// FailKind distinguishes why a poll failed, which determines whether retrying
// could ever help.
type FailKind int

const (
	FailUnreachable FailKind = iota // timeout, refused, no route — expected, retry
	FailAuth                        // credentials rejected — retrying cannot help
	FailBadOutput                   // host answered with something unparseable
)

// NetRate is a per-interface throughput reading in bytes per second.
type NetRate struct {
	Name string
	Rx   float64
	Tx   float64
}

// DiskRate is a per-device throughput reading in bytes per second.
type DiskRate struct {
	Name  string
	Read  float64
	Write float64
}

// HostKind distinguishes a configured machine from an LXD instance discovered
// running on one.
type HostKind int

const (
	KindMachine HostKind = iota
	KindVM
	KindContainer
)

// Host is everything known about one machine: its latest sample, the derived
// values that need two samples to compute, and its trend history.
//
// An LXD guest is a Host too, distinguished only by having a Parent. That is
// the whole reason the tree costs so little: CPU percentage from jiffy diffs,
// network and disk rates, the counter-reset guards protecting all three, and
// the trend rings behind the sparklines are written once here and work on a
// guest unchanged.
type Host struct {
	// Name is the unique key. A guest's is "<parent>/<instance>", because two
	// hosts may each run an instance called "nixos" and the fleet is a flat map.
	Name        string
	Addr        string // what ssh connects to; empty for a guest
	Filesystems []string
	Services    []string
	Containers  []string

	// Parent is the machine this guest runs on, empty for a configured host.
	Parent string
	Kind   HostKind

	Status   Status
	LastSeen time.Time
	LastErr  string
	fails    int

	Cur     Sample
	prev    Sample
	hasPrev bool

	// Derived values. The Has* flags distinguish "not yet computable" (needs a
	// second sample) from a genuine zero.
	CPUPct    float64
	HasCPUPct bool
	NetRates  []NetRate
	HasNet    bool
	DiskRates []DiskRate
	HasDisk   bool

	CPUHist Ring
	MemHist Ring

	// LXD's cumulative CPU-seconds counter, kept only for a guest we could not
	// get inside so the next poll can turn it into a percentage. A guest that
	// answered its own probe reports real jiffies and never touches these.
	guestCPUSecs float64
	guestCores   int
	hasGuestCPU  bool
}

// IsGuest reports whether this row is an LXD instance rather than a configured
// machine.
func (h *Host) IsGuest() bool { return h.Parent != "" }

// Display is the name to show. A guest is only ever drawn underneath its host,
// so its instance name alone is unambiguous there and the qualified key would
// just be noise.
func (h *Host) Display() string {
	if h.Parent == "" {
		return h.Name
	}
	return h.Name[len(h.Parent)+1:]
}

// GuestKey is the fleet-wide unique name for an instance running on a host.
func GuestKey(parent, instance string) string { return parent + "/" + instance }

// TotalNet sums throughput across every interface, for the table's single
// network column.
func (h *Host) TotalNet() (rx, tx float64) {
	for _, r := range h.NetRates {
		rx += r.Rx
		tx += r.Tx
	}
	return rx, tx
}

// Fleet is the full set of monitored hosts in a stable display order. All
// mutation happens on the UI goroutine inside Bubble Tea's Update, which is why
// nothing here needs a mutex.
type Fleet struct {
	Hosts []*Host
	index map[string]*Host
}

// HostRef identifies a host to monitor: what to show it as, and what to
// connect to. Keeping this in model lets config build fleets without model
// having to know the config package exists.
type HostRef struct {
	Name string
	Addr string
	// Watch lists for this host. Empty means no filtering.
	Filesystems []string
	Services    []string
	Containers  []string
}

// NewFleet builds a fleet from configured host references, preserving the
// order they appear in the config file.
func NewFleet(hosts []HostRef) *Fleet {
	f := &Fleet{index: make(map[string]*Host, len(hosts))}
	for _, h := range hosts {
		host := &Host{
			Name: h.Name, Addr: h.Addr,
			Filesystems: h.Filesystems,
			Services:    h.Services,
			Containers:  h.Containers,
			Status:      StatusUnknown,
		}
		f.Hosts = append(f.Hosts, host)
		f.index[h.Name] = host
	}
	return f
}

// Get returns the host with the given display name.
func (f *Fleet) Get(name string) (*Host, bool) {
	h, ok := f.index[name]
	return h, ok
}

// Apply records a successful poll, deriving any values that require comparing
// against the previous sample.
func (f *Fleet) Apply(name string, s Sample) {
	h, ok := f.index[name]
	if !ok {
		return
	}

	h.Status = StatusUp
	h.fails = 0
	h.LastErr = ""
	h.LastSeen = s.At
	s.FS = filterFS(s.FS, h.Filesystems)
	// Reported containers imply the section ran, even from a collector too old
	// to send the sentinel.
	if s.HasContainerInfo || len(s.Containers) > 0 {
		s.Containers = filterContainers(s.Containers, h.Containers)
	} else {
		// Not collected this round: carry the last known state forward rather
		// than reporting everything as gone.
		s.Containers = h.Cur.Containers
	}

	derive(h, s)

	// Guests are reconciled only when the poll actually reported them. A poll
	// that skipped the section is indistinguishable from one where every
	// instance was destroyed, and acting on that would empty the tree.
	if s.HasGuestInfo {
		f.syncGuests(h, s.Guests)
	}
}

// derive records a sample against a host and computes everything that needs
// two points in time. Guests reach it through the same path machines do, which
// is what keeps the counter-reset guards honest for both.
func derive(h *Host, s Sample) {
	if h.hasPrev {
		elapsed := s.At.Sub(h.prev.At).Seconds()
		h.CPUPct, h.HasCPUPct = cpuPercent(h.prev, s)
		h.NetRates, h.HasNet = netRates(h.prev, s, elapsed)
		h.DiskRates, h.HasDisk = diskRates(h.prev, s, elapsed)
	} else {
		// First sample of this host: nothing to diff against yet. Rates fill in
		// one interval from now.
		h.HasCPUPct = false
		h.HasNet = false
		h.HasDisk = false
	}

	h.Cur = s
	h.prev = s
	h.hasPrev = true

	if h.HasCPUPct {
		h.CPUHist.Push(h.CPUPct)
	}
	if s.HasMem {
		h.MemHist.Push(s.MemPct())
	}
}

// syncGuests reconciles the instances discovered on a host against the fleet:
// rows appear for new ones and disappear for those that are gone.
func (f *Fleet) syncGuests(parent *Host, guests []Guest) {
	seen := make(map[string]bool, len(guests))

	for _, g := range guests {
		if g.Name == "" {
			continue
		}
		key := GuestKey(parent.Name, g.Name)
		seen[key] = true

		h, ok := f.index[key]
		if !ok {
			h = &Host{Name: key, Parent: parent.Name, Status: StatusUnknown}
			f.index[key] = h
			f.insertGuest(parent, h)
		}
		if g.Kind == GuestContainer {
			h.Kind = KindContainer
		} else {
			h.Kind = KindVM
		}
		applyGuest(h, g, parent.Cur.At, parent.Cur.Cores)
	}

	f.dropGuests(parent.Name, seen)
}

// insertGuest places a new guest immediately after its host and any siblings it
// already has, so Hosts reads as a tree without needing to be re-sorted.
func (f *Fleet) insertGuest(parent, g *Host) {
	at := len(f.Hosts)
	for i, h := range f.Hosts {
		if h != parent {
			continue
		}
		at = i + 1
		for at < len(f.Hosts) && f.Hosts[at].Parent == parent.Name {
			at++
		}
		break
	}
	f.Hosts = append(f.Hosts, nil)
	copy(f.Hosts[at+1:], f.Hosts[at:])
	f.Hosts[at] = g
}

// dropGuests removes instances of a host that were not in the latest listing.
func (f *Fleet) dropGuests(parent string, keep map[string]bool) {
	out := f.Hosts[:0]
	for _, h := range f.Hosts {
		if h.Parent == parent && !keep[h.Name] {
			delete(f.index, h.Name)
			continue
		}
		out = append(out, h)
	}
	f.Hosts = out
}

// applyGuest records what was learned about one instance.
//
// There are three cases, and they are genuinely different: an instance we got
// inside reports real counters and is treated exactly like a machine; one that
// is running but unreachable has only LXD's accounting to offer; and a stopped
// one has nothing at all.
func applyGuest(h *Host, g Guest, at time.Time, parentCores int) {
	h.LastErr = ""
	h.LastSeen = at

	switch {
	case !g.Running():
		// Not an outage — the instance is doing what it was told. Its last
		// readings are cleared rather than left on screen, where they would
		// imply a machine still running.
		h.Status = StatusStopped
		h.Cur = Sample{At: at}
		h.HasCPUPct, h.HasNet, h.HasDisk = false, false, false
		h.hasPrev, h.hasGuestCPU = false, false

	case g.Probed:
		h.Status = StatusUp
		h.hasGuestCPU = false
		derive(h, g.Sample)

	default:
		// Running, but we could not get inside: a VM with no lxd-agent, or a
		// probe that timed out. LXD's own numbers are all there is.
		h.Status = StatusUp
		h.NetRates, h.HasNet = nil, false
		h.DiskRates, h.HasDisk = nil, false

		s := Sample{At: at}
		if g.MemUsed > 0 && g.MemPct > 0 {
			// LXD reports usage and its percentage but not the total, so the
			// total is recovered from the two. Without it the memory cell could
			// not be drawn in the "used of total" form every other row uses.
			total := uint64(float64(g.MemUsed) / (g.MemPct / 100))
			s.MemTotal, s.MemAvail, s.HasMem = total, total-g.MemUsed, true
		}

		cores := g.Cores
		if cores <= 0 {
			// No CPU limit set, so the instance is free to use every core its
			// host has — which makes the host's count the right divisor.
			cores = parentCores
		}
		h.CPUPct, h.HasCPUPct = guestCPUPercent(h, g.CPUSecs, cores, at)
		h.guestCPUSecs, h.guestCores, h.hasGuestCPU = g.CPUSecs, cores, true

		h.Cur, h.prev, h.hasPrev = s, s, false
		if h.HasCPUPct {
			h.CPUHist.Push(h.CPUPct)
		}
		if s.HasMem {
			h.MemHist.Push(s.MemPct())
		}
	}
}

// guestCPUPercent turns LXD's cumulative CPU-seconds counter into a percentage
// by diffing against the previous poll. It carries the same reset guard the
// jiffy path does: the counter restarts when the instance does, and diffing
// across that boundary would produce a negative rate.
func guestCPUPercent(h *Host, cpuSecs float64, cores int, at time.Time) (float64, bool) {
	if !h.hasGuestCPU || cores <= 0 || h.guestCores != cores {
		return 0, false
	}
	elapsed := at.Sub(h.Cur.At).Seconds()
	delta := cpuSecs - h.guestCPUSecs
	if elapsed <= 0 || delta < 0 {
		return 0, false
	}
	return clampPct(delta / elapsed / float64(cores) * 100), true
}

// Fail records an unsuccessful poll. Last known values and trend history are
// deliberately preserved so you can still see what a machine looked like
// before it went away.
func (f *Fleet) Fail(name string, kind FailKind, msg string) {
	h, ok := f.index[name]
	if !ok {
		return
	}
	h.LastErr = msg

	switch kind {
	case FailAuth:
		// Definitive: no amount of retrying fixes a rejected key, so skip the
		// hysteresis and surface it immediately.
		h.Status = StatusAuth
	case FailBadOutput:
		h.Status = StatusBadOutput
	default:
		h.fails++
		if h.fails >= DownAfterFailures {
			h.Status = StatusDown
		} else {
			h.Status = StatusStale
		}
	}

	// A host that stops responding has no current rates; leaving the old ones
	// on screen would imply traffic that is not happening.
	h.HasCPUPct = false
	h.HasNet = false
	h.HasDisk = false
	h.hasPrev = false

	// Guests are only ever visible through their host, so a host we cannot
	// reach takes its instances with it. Leaving them reading "up" would be a
	// plain lie: we have no idea what they are doing.
	for _, g := range f.Hosts {
		if g.Parent != name {
			continue
		}
		g.LastErr = msg
		g.Status = h.Status
		g.HasCPUPct, g.HasNet, g.HasDisk = false, false, false
		g.hasPrev, g.hasGuestCPU = false, false
	}
}

// cpuPercent derives busy-time percentage from two jiffy snapshots.
func cpuPercent(prev, cur Sample) (float64, bool) {
	if !prev.HasCPU || !cur.HasCPU {
		return 0, false
	}
	totalPrev, totalCur := prev.CPU.Total(), cur.CPU.Total()
	busyPrev, busyCur := prev.CPU.Busy(), cur.CPU.Busy()

	// A decrease means the host rebooted and the counters restarted. Diffing
	// across that boundary would underflow into a nonsense spike, so report
	// nothing for this tick and pick up again on the next one.
	if totalCur <= totalPrev || busyCur < busyPrev {
		return 0, false
	}

	dTotal := float64(totalCur - totalPrev)
	dBusy := float64(busyCur - busyPrev)
	pct := dBusy / dTotal * 100
	return clampPct(pct), true
}

// netRates derives per-interface throughput from two counter snapshots,
// matching interfaces by name so that a NIC appearing or disappearing between
// polls does not shift readings onto the wrong device.
func netRates(prev, cur Sample, elapsed float64) ([]NetRate, bool) {
	if elapsed <= 0 || len(cur.NICs) == 0 {
		return nil, false
	}
	before := make(map[string]NIC, len(prev.NICs))
	for _, n := range prev.NICs {
		before[n.Name] = n
	}

	var out []NetRate
	for _, n := range cur.NICs {
		p, ok := before[n.Name]
		if !ok {
			continue // interface is new; nothing to diff against yet
		}
		// Same reboot/reset guard as CPU: counters only ever climb, so a drop
		// means they were reset underneath us.
		if n.RxBytes < p.RxBytes || n.TxBytes < p.TxBytes {
			continue
		}
		out = append(out, NetRate{
			Name: n.Name,
			Rx:   float64(n.RxBytes-p.RxBytes) / elapsed,
			Tx:   float64(n.TxBytes-p.TxBytes) / elapsed,
		})
	}
	return out, len(out) > 0
}

// diskRates derives per-device throughput from two sector-count snapshots,
// matching devices by name. It carries the same reboot guard as the network
// and CPU counters: a decrease means the counters restarted, and diffing
// across that boundary would underflow into a nonsense spike.
func diskRates(prev, cur Sample, elapsed float64) ([]DiskRate, bool) {
	if elapsed <= 0 || len(cur.Disks) == 0 {
		return nil, false
	}
	before := make(map[string]Disk, len(prev.Disks))
	for _, d := range prev.Disks {
		before[d.Name] = d
	}

	var out []DiskRate
	for _, d := range cur.Disks {
		p, ok := before[d.Name]
		if !ok {
			continue // device is new; nothing to diff against yet
		}
		if d.SectorsRead < p.SectorsRead || d.SectorsWritten < p.SectorsWritten {
			continue
		}
		out = append(out, DiskRate{
			Name:  d.Name,
			Read:  float64(d.SectorsRead-p.SectorsRead) * DiskSectorBytes / elapsed,
			Write: float64(d.SectorsWritten-p.SectorsWritten) * DiskSectorBytes / elapsed,
		})
	}
	return out, len(out) > 0
}

// TotalDisk sums throughput across every device.
func (h *Host) TotalDisk() (read, write float64) {
	for _, d := range h.DiskRates {
		read += d.Read
		write += d.Write
	}
	return read, write
}

// filterFS narrows the reported filesystems to the configured mount points.
// An empty filter keeps everything, so a host you never configured still shows
// a filling disk.
func filterFS(in []FS, keep []string) []FS {
	if len(keep) == 0 || len(in) == 0 {
		return in
	}
	want := make(map[string]bool, len(keep))
	for _, m := range keep {
		want[m] = true
	}
	out := make([]FS, 0, len(keep))
	for _, f := range in {
		if want[f.Mount] {
			out = append(out, f)
		}
	}
	return out
}

// filterContainers narrows the reported containers to the watch list, and
// synthesises an entry for any watched container that is not present at all.
// A container that was deleted, or never created after a rebuild, is exactly
// the failure this list exists to catch — silently omitting it would leave the
// display looking healthy.
func filterContainers(in []Container, keep []string) []Container {
	if len(keep) == 0 {
		return in
	}
	byName := make(map[string]Container, len(in))
	for _, c := range in {
		byName[c.Name] = c
	}

	out := make([]Container, 0, len(keep))
	for _, name := range keep {
		if c, ok := byName[name]; ok {
			out = append(out, c)
			continue
		}
		out = append(out, Container{Name: name, State: ContainerMissing})
	}
	return out
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
