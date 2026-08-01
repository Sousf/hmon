package collect

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Sousf/hmon/internal/model"
)

// FormatVersion is the collector output format this build understands. The
// script emits it as its first line so a mismatched or partially written
// script is rejected outright rather than misread column-by-column.
const FormatVersion = "1"

// ErrBadOutput means the host answered but with something we cannot trust.
// It is distinct from an unreachable host: the machine is alive, so this
// points at a bug rather than at the network.
var ErrBadOutput = errors.New("unparseable collector output")

// Parse turns collector stdout into a Sample.
//
// Unknown keys are ignored rather than rejected, so adding a metric to the
// script does not break clients built before it existed. Malformed values
// within a known key are skipped individually — one bad temperature sensor
// should not discard an otherwise good sample.
func Parse(out []byte, at time.Time) (model.Sample, error) {
	s := model.Sample{At: at}
	guests := newGuestSet(at)

	var sawVersion, sawEnd bool
	sc := bufio.NewScanner(bytes.NewReader(out))
	// Process command lines can be long; the default 64K limit is generous but
	// make the ceiling explicit.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		key, args := fields[0], fields[1:]

		switch key {
		case "v":
			if len(args) < 1 || args[0] != FormatVersion {
				return model.Sample{}, fmt.Errorf("%w: unsupported format version %q", ErrBadOutput, strings.Join(args, " "))
			}
			sawVersion = true

		case "end":
			sawEnd = true

		case "guestsreported":
			// Same role as containersreported: proof the section ran, so a poll
			// that skipped it is never read as one where every instance vanished.
			s.HasGuestInfo = true

		case "guest":
			guests.declare(args)

		case "guestcores":
			guests.setCores(args)

		case "g":
			guests.line(args)

		default:
			applyLine(&s, key, args)
		}
	}
	if err := sc.Err(); err != nil {
		return model.Sample{}, fmt.Errorf("%w: %v", ErrBadOutput, err)
	}

	if !sawVersion {
		return model.Sample{}, fmt.Errorf("%w: missing version line", ErrBadOutput)
	}
	// The trailing sentinel is what distinguishes a complete sample from one
	// truncated by a dropped connection mid-write. Without it we could apply
	// half a reading and show a host as having lost its filesystems.
	if !sawEnd {
		return model.Sample{}, fmt.Errorf("%w: output truncated", ErrBadOutput)
	}

	s.Guests = guests.finish()
	s.Procs = dedupeProcs(s.Procs)
	return s, nil
}

// applyLine folds one "key value..." line into a sample.
//
// It is shared between a host's own reading and a guest's, which arrives
// prefixed but is otherwise byte-identical — the same collector produced both.
// Unknown keys fall through silently, so adding a metric to the script does not
// break clients built before it existed.
func applyLine(s *model.Sample, key string, args []string) {
	switch key {
	case "uptime":
		if len(args) >= 1 {
			if secs, err := strconv.ParseFloat(args[0], 64); err == nil {
				s.Uptime = time.Duration(secs * float64(time.Second))
			}
		}

	case "cpu":
		if c, ok := parseCPU(args); ok {
			s.CPU, s.HasCPU = c, true
		}

	case "mem":
		// Reported in kilobytes, straight from /proc/meminfo. Scaling to
		// bytes happens here rather than in awk, which cannot represent the
		// result exactly on hosts with more than 2GB.
		if len(args) >= 2 {
			totalKB, err1 := strconv.ParseUint(args[0], 10, 64)
			availKB, err2 := strconv.ParseUint(args[1], 10, 64)
			if err1 == nil && err2 == nil && totalKB > 0 {
				s.MemTotal, s.MemAvail, s.HasMem = totalKB*1024, availKB*1024, true
			}
		}

	case "swap":
		// Also kilobytes from /proc/meminfo, for the same awk reason as mem.
		if len(args) >= 2 {
			totalKB, err1 := strconv.ParseUint(args[0], 10, 64)
			freeKB, err2 := strconv.ParseUint(args[1], 10, 64)
			if err1 == nil && err2 == nil {
				s.SwapTotal, s.SwapFree = totalKB*1024, freeKB*1024
			}
		}

	case "cores":
		if len(args) >= 1 {
			if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
				s.Cores = n
			}
		}

	case "failedcount":
		// Emitted even when zero, which is what distinguishes "systemd is
		// present and nothing is failing" from "this host has no systemd".
		if len(args) >= 1 {
			if _, err := strconv.Atoi(args[0]); err == nil {
				s.HasUnitInfo = true
			}
		}

	case "failed":
		s.FailedUnits = append(s.FailedUnits, args...)

	case "rebootrequired":
		s.RebootRequired = true

	case "containersreported":
		// Present whenever the containers section actually ran, which is
		// what distinguishes "this host has none" from "we did not ask".
		s.HasContainerInfo = true

	case "svc":
		// load-state, active-state, then the name, which is terminal so it
		// can contain anything systemd allows.
		if len(args) >= 3 {
			s.Services = append(s.Services, model.Service{
				LoadState:   args[0],
				ActiveState: args[1],
				Name:        strings.Join(args[2:], " "),
			})
		}

	case "container":
		if len(args) >= 3 {
			s.Containers = append(s.Containers, model.Container{
				Runtime: args[0],
				State:   args[1],
				Name:    strings.Join(args[2:], " "),
			})
		}

	case "disk":
		if len(args) >= 3 {
			read, err1 := strconv.ParseUint(args[1], 10, 64)
			written, err2 := strconv.ParseUint(args[2], 10, 64)
			if err1 == nil && err2 == nil {
				s.Disks = append(s.Disks, model.Disk{
					Name: args[0], SectorsRead: read, SectorsWritten: written,
				})
			}
		}

	case "load":
		if len(args) >= 3 {
			for i := 0; i < 3; i++ {
				if v, err := strconv.ParseFloat(args[i], 64); err == nil {
					s.Load[i] = v
				}
			}
		}

	case "fs":
		if len(args) >= 4 {
			total, err1 := strconv.ParseUint(args[1], 10, 64)
			used, err2 := strconv.ParseUint(args[2], 10, 64)
			avail, err3 := strconv.ParseUint(args[3], 10, 64)
			if err1 == nil && err2 == nil && err3 == nil {
				s.FS = append(s.FS, model.FS{
					Mount: args[0], TotalKB: total, UsedKB: used, AvailKB: avail,
				})
			}
		}

	case "net":
		if len(args) >= 3 {
			rx, err1 := strconv.ParseUint(args[1], 10, 64)
			tx, err2 := strconv.ParseUint(args[2], 10, 64)
			if err1 == nil && err2 == nil {
				s.NICs = append(s.NICs, model.NIC{Name: args[0], RxBytes: rx, TxBytes: tx})
			}
		}

	case "temp":
		// Value first, label last — sensor labels contain spaces.
		if len(args) >= 2 {
			if c, err := strconv.ParseFloat(args[0], 64); err == nil && plausibleTemp(c) {
				s.Temps = append(s.Temps, model.Temp{Label: strings.Join(args[1:], " "), C: c})
			}
		}

	case "proc":
		if p, ok := parseProc(args); ok {
			s.Procs = append(s.Procs, p)
		}
	}
}

// guestSet accumulates the guest section as its lines arrive.
//
// Each instance is declared once from LXD's listing and may then be followed by
// a sub-document of its own — the output of this same collector, run inside it.
// That sub-document's version and end sentinels are checked exactly as the
// host's are, so a probe cut short halfway is discarded whole rather than
// applied in part.
type guestSet struct {
	at     time.Time
	order  []string
	byName map[string]*guestBuf
}

type guestBuf struct {
	g          model.Guest
	sample     model.Sample
	sawVersion bool
	sawEnd     bool
}

func newGuestSet(at time.Time) *guestSet {
	return &guestSet{at: at, byName: make(map[string]*guestBuf)}
}

func (gs *guestSet) get(name string) *guestBuf {
	if b, ok := gs.byName[name]; ok {
		return b
	}
	b := &guestBuf{}
	gs.byName[name] = b
	gs.order = append(gs.order, name)
	return b
}

// declare records one row of the LXD listing: name, kind, state, and the
// daemon's own accounting.
func (gs *guestSet) declare(args []string) {
	if len(args) < 3 {
		return
	}
	b := gs.get(args[0])
	if args[1] == string(model.GuestVM) {
		b.g.Kind = model.GuestVM
	} else {
		b.g.Kind = model.GuestContainer
	}
	b.g.State = args[2]

	// Everything past the state is best-effort: LXD leaves these blank for an
	// instance it cannot account for, and the collector sends "-" in their
	// place so the columns stay aligned.
	if len(args) > 3 {
		b.g.MemUsed, _ = parseIECBytes(args[3])
	}
	if len(args) > 4 {
		b.g.MemPct, _ = parsePercent(args[4])
	}
	if len(args) > 5 {
		b.g.CPUSecs, _ = parseSeconds(args[5])
	}
	if len(args) > 6 {
		if n, err := strconv.Atoi(args[6]); err == nil && n >= 0 {
			b.g.Procs = n
		}
	}
}

// setCores records the CPU limit of an instance we could not get inside, which
// is the divisor that turns LXD's cumulative CPU-seconds into a percentage.
func (gs *guestSet) setCores(args []string) {
	if len(args) < 2 {
		return
	}
	if n, err := strconv.Atoi(args[1]); err == nil && n >= 0 {
		gs.get(args[0]).g.Cores = n
	}
}

// line folds one line of a guest's own sub-document into its sample.
func (gs *guestSet) line(args []string) {
	if len(args) < 2 {
		return
	}
	b := gs.get(args[0])
	key, rest := args[1], args[2:]
	switch key {
	case "v":
		b.sawVersion = len(rest) > 0 && rest[0] == FormatVersion
	case "end":
		b.sawEnd = true
	default:
		applyLine(&b.sample, key, rest)
	}
}

func (gs *guestSet) finish() []model.Guest {
	if len(gs.order) == 0 {
		return nil
	}
	out := make([]model.Guest, 0, len(gs.order))
	for _, name := range gs.order {
		b := gs.byName[name]
		g := b.g
		g.Name = name
		// Both sentinels or nothing: a partial probe is worse than none, since
		// it would render as a machine that has lost half its filesystems.
		if b.sawVersion && b.sawEnd {
			b.sample.At = gs.at
			b.sample.Procs = dedupeProcs(b.sample.Procs)
			g.Sample, g.Probed = b.sample, true
		}
		out = append(out, g)
	}
	return out
}

// parseIECBytes reads LXD's human-formatted byte counts — "512B", "267.44MiB",
// "1.05GiB". The daemon offers no machine-readable form for these columns, so
// the formatting has to be undone here rather than avoided.
func parseIECBytes(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0, false
	}
	i := strings.IndexFunc(s, func(r rune) bool {
		return r != '.' && (r < '0' || r > '9')
	})
	if i < 0 {
		i = len(s)
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || num < 0 {
		return 0, false
	}
	mult, ok := iecMultiplier(strings.TrimSuffix(s[i:], "B"))
	if !ok {
		return 0, false
	}
	return uint64(num * mult), true
}

func iecMultiplier(prefix string) (float64, bool) {
	switch strings.ToLower(prefix) {
	case "":
		return 1, true
	case "ki":
		return 1 << 10, true
	case "mi":
		return 1 << 20, true
	case "gi":
		return 1 << 30, true
	case "ti":
		return 1 << 40, true
	case "pi":
		return 1 << 50, true
	case "ei":
		return 1 << 60, true
	}
	return 0, false
}

// parsePercent reads LXD's "6.5%" form.
func parsePercent(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// parseSeconds reads LXD's CPU-usage column. It is rendered as a duration —
// "11s" for a fresh instance, "3h25m45s" for one that has been busy a while —
// so it is parsed as one rather than assumed to be bare seconds.
func parseSeconds(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, false
	}
	return d.Seconds(), true
}

// plausibleTemp rejects readings from sensors that are present but not
// actually reporting. ACPI in particular emits -273.1 (absolute zero) for a
// disabled sensor, which would otherwise render as a real measurement.
func plausibleTemp(c float64) bool {
	return c > -50 && c < 200
}

func parseCPU(args []string) (model.CPUTimes, bool) {
	if len(args) < 4 {
		return model.CPUTimes{}, false
	}
	var vals [8]uint64
	for i := 0; i < len(vals) && i < len(args); i++ {
		v, err := strconv.ParseUint(args[i], 10, 64)
		if err != nil {
			return model.CPUTimes{}, false
		}
		vals[i] = v
	}
	return model.CPUTimes{
		User: vals[0], Nice: vals[1], System: vals[2], Idle: vals[3],
		IOWait: vals[4], IRQ: vals[5], SoftIRQ: vals[6], Steal: vals[7],
	}, true
}

func parseProc(args []string) (model.Proc, bool) {
	if len(args) < 4 {
		return model.Proc{}, false
	}
	pid, err1 := strconv.Atoi(args[0])
	cpu, err2 := strconv.ParseFloat(args[1], 64)
	rss, err3 := strconv.ParseUint(args[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return model.Proc{}, false
	}
	// Command is the terminal field and may contain spaces, so take everything
	// remaining verbatim.
	cmd := strings.Join(args[3:], " ")
	return model.Proc{PID: pid, CPUPct: cpu, RSSKB: rss, Command: cmd}, true
}

// dedupeProcs removes repeats by PID. The collector deliberately sends the
// union of the top processes by CPU and by memory so the client can re-sort
// either way, which means processes high in both rankings arrive twice.
func dedupeProcs(in []model.Proc) []model.Proc {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[int]bool, len(in))
	out := in[:0]
	for _, p := range in {
		if seen[p.PID] {
			continue
		}
		seen[p.PID] = true
		out = append(out, p)
	}
	return out
}
