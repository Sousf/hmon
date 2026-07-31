// Package model holds hmon's shared vocabulary: the raw samples
// collectors produce, and the fleet state the UI renders. It performs no I/O.
package model

import "time"

// Status classifies what we currently know about a host. Failures are kept
// distinct because they call for different responses: a down host is usually
// expected, an auth failure means the local config is wrong, and bad output
// means the host answered but the collector misbehaved.
type Status int

const (
	StatusUnknown Status = iota // never polled yet
	StatusUp
	StatusStale // one failed poll; last known values still shown
	StatusDown  // two or more consecutive failures
	StatusAuth  // authentication rejected — a configuration problem
	StatusBadOutput
)

// Live reports whether the host is currently reporting readings worth
// displaying. A stale host still counts: it missed one poll but its last
// values are recent enough to mean something.
func (s Status) Live() bool {
	return s == StatusUp || s == StatusStale
}

func (s Status) String() string {
	switch s {
	case StatusUp:
		return "up"
	case StatusStale:
		return "stale"
	case StatusDown:
		return "down"
	case StatusAuth:
		return "auth"
	case StatusBadOutput:
		return "bad output"
	default:
		return "—"
	}
}

// FS is one mounted filesystem as reported by df.
type FS struct {
	Mount   string
	TotalKB uint64
	UsedKB  uint64
	AvailKB uint64
}

// UsedPct matches df's own Use% column: usage as a fraction of space actually
// available to you, which excludes root-reserved blocks. Dividing by TotalKB
// instead would report a few percent high on a default ext4.
func (f FS) UsedPct() float64 {
	denom := f.UsedKB + f.AvailKB
	if denom == 0 {
		return 0
	}
	return float64(f.UsedKB) / float64(denom) * 100
}

// NIC holds the cumulative byte counters for one interface. Rates are derived
// later by Fleet.Apply; the collector never computes them.
type NIC struct {
	Name    string
	RxBytes uint64
	TxBytes uint64
}

// Disk holds cumulative sector counters for one block device. Sectors in
// /proc/diskstats are always 512 bytes regardless of the device's physical
// block size.
type Disk struct {
	Name           string
	SectorsRead    uint64
	SectorsWritten uint64
}

// DiskSectorBytes is the fixed sector size /proc/diskstats reports in.
const DiskSectorBytes = 512

// Temp is a single temperature sensor reading in degrees Celsius.
type Temp struct {
	Label string
	C     float64
}

// Proc is one process, only ever populated for the host being viewed in detail.
// CPUPct is a true instantaneous measurement: the collector samples
// /proc/<pid>/stat twice and diffs, rather than using ps's lifetime average.
type Proc struct {
	PID     int
	CPUPct  float64
	RSSKB   uint64
	Command string
}

// CPUTimes are the raw jiffy counters from the first line of /proc/stat.
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Total is every jiffy accounted for, busy or otherwise.
func (c CPUTimes) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// Busy is every jiffy not spent idle or waiting on I/O.
func (c CPUTimes) Busy() uint64 {
	return c.Total() - c.Idle - c.IOWait
}

// Sample is one poll's worth of raw readings from a host. Counters are stored
// exactly as read so that the collector stays stateless and testable; anything
// requiring two points in time is computed in Fleet.Apply.
type Sample struct {
	At       time.Time
	Uptime   time.Duration
	CPU      CPUTimes
	MemTotal uint64 // bytes
	MemAvail uint64 // bytes
	Load     [3]float64
	FS       []FS
	NICs     []NIC
	Disks    []Disk
	Temps    []Temp
	Procs    []Proc

	// Cores is the CPU count, without which a load average cannot be read: 4.0
	// is idle on a 16-core box and a crisis on a dual-core Pi.
	Cores int

	SwapTotal uint64 // bytes; zero means no swap configured
	SwapFree  uint64

	// FailedUnits are systemd units in the failed state. This is the only
	// signal here that catches a crashed service — a host with a dead postgres
	// looks perfectly healthy on CPU and memory.
	FailedUnits []string
	// HasUnitInfo distinguishes "no failed units" from "systemd not present",
	// so a non-systemd host shows nothing rather than a misleading all-clear.
	HasUnitInfo bool

	RebootRequired bool

	// HasCPU and HasMem record whether those readings were present at all, so
	// a host that omits them renders "n/a" rather than a convincing zero.
	HasCPU bool
	HasMem bool
}

// SwapUsed is swap in use, in bytes.
func (s Sample) SwapUsed() uint64 {
	if s.SwapFree > s.SwapTotal {
		return 0
	}
	return s.SwapTotal - s.SwapFree
}

// SwapPct is swap in use as a percentage of total, or zero when no swap is
// configured.
func (s Sample) SwapPct() float64 {
	if s.SwapTotal == 0 {
		return 0
	}
	return float64(s.SwapUsed()) / float64(s.SwapTotal) * 100
}

// LoadPerCore expresses the one-minute load average as a fraction of available
// cores, which is the form that means the same thing on every machine: 1.0 is
// fully committed regardless of core count.
func (s Sample) LoadPerCore() (float64, bool) {
	if s.Cores <= 0 {
		return 0, false
	}
	return s.Load[0] / float64(s.Cores), true
}

// MemUsed is total minus available. MemAvailable already accounts for
// reclaimable cache, so this is the number that matches what you would call
// "used" in practice, not total-minus-free.
func (s Sample) MemUsed() uint64 {
	if s.MemAvail > s.MemTotal {
		return 0
	}
	return s.MemTotal - s.MemAvail
}

// MemPct is memory in use as a percentage of total.
func (s Sample) MemPct() float64 {
	if s.MemTotal == 0 {
		return 0
	}
	return float64(s.MemUsed()) / float64(s.MemTotal) * 100
}

// MaxTemp returns the hottest sensor reading, which is the one worth putting in
// a single-column table. ok is false when the host exposes no sensors.
func (s Sample) MaxTemp() (t Temp, ok bool) {
	for _, c := range s.Temps {
		if !ok || c.C > t.C {
			t, ok = c, true
		}
	}
	return t, ok
}

// RootFS returns the filesystem to show in the table's single disk column,
// preferring "/" and otherwise falling back to the fullest one.
func (s Sample) RootFS() (FS, bool) {
	if len(s.FS) == 0 {
		return FS{}, false
	}
	best := s.FS[0]
	for _, f := range s.FS {
		if f.Mount == "/" {
			return f, true
		}
		if f.UsedPct() > best.UsedPct() {
			best = f
		}
	}
	return best, true
}
