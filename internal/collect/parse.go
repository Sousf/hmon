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

	s.Procs = dedupeProcs(s.Procs)
	return s, nil
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
