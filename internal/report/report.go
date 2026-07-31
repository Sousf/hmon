// Package report renders fleet state as JSON for one-shot, non-interactive
// use.
//
// This is how hmon stays a dashboard rather than becoming a monitoring
// service: instead of growing notifications, thresholds-with-actions, and a
// daemon, it can be run from cron and piped into whatever alerting you already
// have.
package report

import (
	"encoding/json"
	"io"
	"time"

	"github.com/Sousf/hmon/internal/model"
)

// Fleet is the top-level document.
type Fleet struct {
	GeneratedAt time.Time `json:"generated_at"`
	Hosts       []Host    `json:"hosts"`
}

// Host is one machine's state. Fields that need two samples, or that a host
// did not report, are omitted rather than sent as a zero that would read as a
// real measurement.
type Host struct {
	Name   string `json:"name"`
	Addr   string `json:"addr"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	UptimeSeconds float64 `json:"uptime_seconds,omitempty"`
	Cores         int     `json:"cores,omitempty"`

	CPUPct    *float64    `json:"cpu_pct,omitempty"`
	Load      *[3]float64 `json:"load,omitempty"`
	MemUsed   uint64      `json:"mem_used_bytes,omitempty"`
	MemTotal  uint64      `json:"mem_total_bytes,omitempty"`
	SwapUsed  uint64      `json:"swap_used_bytes,omitempty"`
	SwapTotal uint64      `json:"swap_total_bytes,omitempty"`

	Filesystems []Filesystem `json:"filesystems,omitempty"`
	Network     []Rate       `json:"network,omitempty"`
	Disks       []Rate       `json:"disk_io,omitempty"`
	Temps       []Temp       `json:"temperatures,omitempty"`

	FailedUnits    []string    `json:"failed_units,omitempty"`
	Services       []Service   `json:"services,omitempty"`
	Containers     []Container `json:"containers,omitempty"`
	RebootRequired bool        `json:"reboot_required,omitempty"`

	// Healthy is the single field a cron job would branch on. It is false when
	// anything the operator asked about is wrong: the host is not up, a watched
	// service or container is not running, or a unit has failed.
	Healthy bool `json:"healthy"`
}

type Filesystem struct {
	Mount   string  `json:"mount"`
	UsedPct float64 `json:"used_pct"`
	UsedKB  uint64  `json:"used_kb"`
	AvailKB uint64  `json:"avail_kb"`
	TotalKB uint64  `json:"total_kb"`
}

type Rate struct {
	Name string  `json:"name"`
	In   float64 `json:"in_bytes_per_sec"`
	Out  float64 `json:"out_bytes_per_sec"`
}

type Temp struct {
	Label   string  `json:"label"`
	Celsius float64 `json:"celsius"`
}

type Service struct {
	Name        string `json:"name"`
	LoadState   string `json:"load_state"`
	ActiveState string `json:"active_state"`
}

type Container struct {
	Runtime string `json:"runtime,omitempty"`
	Name    string `json:"name"`
	State   string `json:"state"`
}

// Build converts fleet state into the report document.
func Build(f *model.Fleet, at time.Time) Fleet {
	out := Fleet{GeneratedAt: at, Hosts: make([]Host, 0, len(f.Hosts))}
	for _, h := range f.Hosts {
		out.Hosts = append(out.Hosts, buildHost(h))
	}
	return out
}

func buildHost(h *model.Host) Host {
	s := h.Cur
	host := Host{
		Name:           h.Name,
		Addr:           h.Addr,
		Status:         h.Status.String(),
		Error:          h.LastErr,
		Cores:          s.Cores,
		MemUsed:        s.MemUsed(),
		MemTotal:       s.MemTotal,
		SwapUsed:       s.SwapUsed(),
		SwapTotal:      s.SwapTotal,
		FailedUnits:    s.FailedUnits,
		RebootRequired: s.RebootRequired,
	}
	if s.Uptime > 0 {
		host.UptimeSeconds = s.Uptime.Seconds()
	}
	// Pointers for the values that genuinely may not exist yet: a zero CPU
	// percentage is a real reading, and must not be confused with "not
	// computable from one sample".
	if h.HasCPUPct {
		v := h.CPUPct
		host.CPUPct = &v
	}
	if s.Cores > 0 || s.Load != [3]float64{} {
		l := s.Load
		host.Load = &l
	}

	for _, fs := range s.FS {
		host.Filesystems = append(host.Filesystems, Filesystem{
			Mount: fs.Mount, UsedPct: fs.UsedPct(),
			UsedKB: fs.UsedKB, AvailKB: fs.AvailKB, TotalKB: fs.TotalKB,
		})
	}
	for _, r := range h.NetRates {
		host.Network = append(host.Network, Rate{Name: r.Name, In: r.Rx, Out: r.Tx})
	}
	for _, d := range h.DiskRates {
		host.Disks = append(host.Disks, Rate{Name: d.Name, In: d.Read, Out: d.Write})
	}
	for _, t := range s.Temps {
		host.Temps = append(host.Temps, Temp{Label: t.Label, Celsius: t.C})
	}
	for _, svc := range s.Services {
		host.Services = append(host.Services, Service{
			Name: svc.Name, LoadState: svc.LoadState, ActiveState: svc.ActiveState,
		})
	}
	for _, c := range s.Containers {
		host.Containers = append(host.Containers, Container{
			Runtime: c.Runtime, Name: c.Name, State: c.State,
		})
	}

	host.Healthy = h.Status == model.StatusUp &&
		len(s.FailedUnits) == 0 &&
		len(s.StoppedServices()) == 0 &&
		len(s.StoppedContainers()) == 0

	return host
}

// Write emits the report as indented JSON, which stays greppable by eye while
// remaining valid for jq.
func Write(w io.Writer, f Fleet) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}
