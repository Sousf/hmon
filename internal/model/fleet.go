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

// Host is everything known about one machine: its latest sample, the derived
// values that need two samples to compute, and its trend history.
type Host struct {
	Name        string // display name
	Addr        string // what ssh connects to
	Filesystems []string

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

	CPUHist Ring
	MemHist Ring
}

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
	// Filesystems narrows which mount points to keep. Empty means keep all.
	Filesystems []string
}

// NewFleet builds a fleet from configured host references, preserving the
// order they appear in the config file.
func NewFleet(hosts []HostRef) *Fleet {
	f := &Fleet{index: make(map[string]*Host, len(hosts))}
	for _, h := range hosts {
		host := &Host{
			Name: h.Name, Addr: h.Addr,
			Filesystems: h.Filesystems, Status: StatusUnknown,
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

	if h.hasPrev {
		elapsed := s.At.Sub(h.prev.At).Seconds()
		h.CPUPct, h.HasCPUPct = cpuPercent(h.prev, s)
		h.NetRates, h.HasNet = netRates(h.prev, s, elapsed)
	} else {
		// First sample of this host: nothing to diff against yet. Rates fill in
		// one interval from now.
		h.HasCPUPct = false
		h.HasNet = false
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
	h.hasPrev = false
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

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
