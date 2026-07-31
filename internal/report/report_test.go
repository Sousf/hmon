package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Sousf/hmon/internal/model"
)

// fleetWith builds a single-host fleet and applies the given samples in order,
// which is how derived values like CPU percentage come to exist.
func fleetWith(t *testing.T, ref model.HostRef, samples ...model.Sample) *model.Fleet {
	t.Helper()
	f := model.NewFleet([]model.HostRef{ref})
	for _, s := range samples {
		f.Apply(ref.Name, s)
	}
	return f
}

func healthySample(at time.Time) model.Sample {
	return model.Sample{
		At:               at,
		Uptime:           26 * time.Hour,
		HasCPU:           true,
		HasMem:           true,
		HasContainerInfo: true,
		HasUnitInfo:      true,
		Cores:            16,
		Load:             [3]float64{0.5, 0.4, 0.3},
		MemTotal:         16 << 30,
		MemAvail:         12 << 30,
		CPU:              model.CPUTimes{User: 1000, Idle: 9000},
		FS:               []model.FS{{Mount: "/", TotalKB: 1000, UsedKB: 400, AvailKB: 600}},
		NICs:             []model.NIC{{Name: "eth0", RxBytes: 1000, TxBytes: 500}},
		Disks:            []model.Disk{{Name: "nvme0n1", SectorsRead: 100, SectorsWritten: 200}},
		Temps:            []model.Temp{{Label: "Package id 0", C: 41}},
	}
}

func TestBuildMapsHostFields(t *testing.T) {
	s1 := healthySample(time.Unix(100, 0))
	s2 := healthySample(time.Unix(102, 0))
	s2.CPU = model.CPUTimes{User: 1500, Idle: 9500} // 50% busy over the interval
	s2.NICs = []model.NIC{{Name: "eth0", RxBytes: 3000, TxBytes: 1500}}
	s2.Disks = []model.Disk{{Name: "nvme0n1", SectorsRead: 500, SectorsWritten: 400}}

	f := fleetWith(t, model.HostRef{Name: "nas", Addr: "nas.lan"}, s1, s2)
	got := Build(f, time.Unix(200, 0))

	if len(got.Hosts) != 1 {
		t.Fatalf("hosts = %d, want 1", len(got.Hosts))
	}
	h := got.Hosts[0]

	if h.Name != "nas" || h.Addr != "nas.lan" {
		t.Errorf("name/addr = %q/%q, want nas/nas.lan", h.Name, h.Addr)
	}
	if h.Status != "up" {
		t.Errorf("status = %q, want up", h.Status)
	}
	if h.Cores != 16 {
		t.Errorf("cores = %d, want 16", h.Cores)
	}
	if h.UptimeSeconds != (26 * time.Hour).Seconds() {
		t.Errorf("uptime = %v, want %v", h.UptimeSeconds, (26 * time.Hour).Seconds())
	}
	if h.MemTotal != 16<<30 || h.MemUsed != 4<<30 {
		t.Errorf("mem used/total = %d/%d, want %d/%d", h.MemUsed, h.MemTotal, 4<<30, 16<<30)
	}

	if h.CPUPct == nil {
		t.Fatal("cpu_pct is nil after two samples, want a value")
	}
	if *h.CPUPct < 49 || *h.CPUPct > 51 {
		t.Errorf("cpu_pct = %v, want ~50", *h.CPUPct)
	}
	if h.Load == nil || (*h.Load)[0] != 0.5 {
		t.Errorf("load = %v, want [0.5 0.4 0.3]", h.Load)
	}

	if len(h.Filesystems) != 1 || h.Filesystems[0].Mount != "/" {
		t.Fatalf("filesystems = %+v", h.Filesystems)
	}
	if pct := h.Filesystems[0].UsedPct; pct < 39 || pct > 41 {
		t.Errorf("used_pct = %v, want 40", pct)
	}

	// Rates are derived, so they only appear once there are two samples.
	if len(h.Network) != 1 || h.Network[0].Name != "eth0" || h.Network[0].In <= 0 {
		t.Errorf("network = %+v, want a positive eth0 rate", h.Network)
	}
	if len(h.Disks) != 1 || h.Disks[0].In <= 0 {
		t.Errorf("disk_io = %+v, want a positive read rate", h.Disks)
	}
	if len(h.Temps) != 1 || h.Temps[0].Label != "Package id 0" {
		t.Errorf("temperatures = %+v", h.Temps)
	}

	if got.GeneratedAt != time.Unix(200, 0) {
		t.Errorf("generated_at = %v, want the time passed in", got.GeneratedAt)
	}
}

// TestZeroCPUIsReportedNotOmitted is the reason CPUPct is a pointer. A host
// that is genuinely idle must serialise 0, while a host with only one sample
// must omit the field entirely — a consumer cannot tell those apart if both
// come through as a plain zero.
func TestZeroCPUIsReportedNotOmitted(t *testing.T) {
	idle1 := healthySample(time.Unix(100, 0))
	idle2 := healthySample(time.Unix(102, 0))
	idle2.CPU = model.CPUTimes{User: 1000, Idle: 10000} // no busy jiffies at all

	f := fleetWith(t, model.HostRef{Name: "nas", Addr: "nas"}, idle1, idle2)
	body := mustJSON(t, Build(f, time.Unix(200, 0)))

	if !strings.Contains(body, `"cpu_pct": 0`) {
		t.Errorf("idle host omitted cpu_pct instead of reporting zero:\n%s", body)
	}

	// One sample: nothing to diff against, so the field must be absent.
	one := fleetWith(t, model.HostRef{Name: "nas", Addr: "nas"}, healthySample(time.Unix(100, 0)))
	body = mustJSON(t, Build(one, time.Unix(200, 0)))
	if strings.Contains(body, "cpu_pct") {
		t.Errorf("single-sample host reported cpu_pct, want it omitted:\n%s", body)
	}
}

// TestHealthyAcrossFailureModes pins down the one field a cron job branches on.
func TestHealthyAcrossFailureModes(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*model.Sample)
		fail    *model.FailKind
		failN   int
		want    bool
		because string
	}{
		{
			name: "everything fine",
			want: true,
		},
		{
			name:    "failed unit",
			mutate:  func(s *model.Sample) { s.FailedUnits = []string{"openipmi.service"} },
			want:    false,
			because: "a failed unit is invisible in every resource column",
		},
		{
			name: "watched service stopped",
			mutate: func(s *model.Sample) {
				s.Services = []model.Service{
					{Name: "postgresql", LoadState: "loaded", ActiveState: "inactive"},
				}
			},
			want:    false,
			because: "stopped is not the same as failed, and systemd reports neither",
		},
		{
			name: "watched service does not exist",
			mutate: func(s *model.Sample) {
				s.Services = []model.Service{
					{Name: "caddy", LoadState: "not-found", ActiveState: "inactive"},
				}
			},
			want:    true,
			because: "a unit that runs in a container is a config mismatch, not an outage",
		},
		{
			name: "container stopped",
			mutate: func(s *model.Sample) {
				s.Containers = []model.Container{{Name: "web", State: "exited"}}
			},
			want: false,
		},
		{
			name: "container missing",
			mutate: func(s *model.Sample) {
				s.Containers = []model.Container{{Name: "web", State: model.ContainerMissing}}
			},
			want: false,
		},
		{
			name: "container running",
			mutate: func(s *model.Sample) {
				s.Containers = []model.Container{{Name: "web", State: "running"}}
			},
			want: true,
		},
		{
			name:    "reboot pending",
			mutate:  func(s *model.Sample) { s.RebootRequired = true },
			want:    true,
			because: "a pending reboot is hygiene, not an outage worth paging on",
		},
		{
			name:  "host down",
			fail:  kind(model.FailUnreachable),
			failN: 2,
			want:  false,
		},
		{
			name:  "auth rejected",
			fail:  kind(model.FailAuth),
			failN: 1,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := healthySample(time.Unix(100, 0))
			if tt.mutate != nil {
				tt.mutate(&s)
			}
			f := fleetWith(t, model.HostRef{Name: "nas", Addr: "nas"}, s)
			for i := 0; i < tt.failN; i++ {
				f.Fail("nas", *tt.fail, "boom")
			}

			got := Build(f, time.Unix(200, 0)).Hosts[0]
			if got.Healthy != tt.want {
				t.Errorf("healthy = %v, want %v (%s)", got.Healthy, tt.want, tt.because)
			}
		})
	}
}

// TestStaleHostIsNotHealthy documents a judgement call rather than an
// obviously right answer: one missed poll marks a host stale, and stale does
// not count as healthy. In -once mode only two polls happen, so stale means
// half of them failed — worth surfacing. The cost is that a single SSH hiccup
// can produce one unhealthy reading.
func TestStaleHostIsNotHealthy(t *testing.T) {
	f := fleetWith(t, model.HostRef{Name: "nas", Addr: "nas"}, healthySample(time.Unix(100, 0)))
	f.Fail("nas", model.FailUnreachable, "timeout")

	h := Build(f, time.Unix(200, 0)).Hosts[0]
	if h.Status != "stale" {
		t.Fatalf("status = %q, want stale", h.Status)
	}
	if h.Healthy {
		t.Error("stale host reported healthy; if this is changed, note that a single " +
			"dropped poll would then be invisible to a cron check")
	}
}

func TestErrorSurfacedForFailedHost(t *testing.T) {
	f := fleetWith(t, model.HostRef{Name: "nas", Addr: "nas"})
	f.Fail("nas", model.FailAuth, "Permission denied (publickey)")

	h := Build(f, time.Unix(200, 0)).Hosts[0]
	if h.Status != "auth" {
		t.Errorf("status = %q, want auth", h.Status)
	}
	if !strings.Contains(h.Error, "Permission denied") {
		t.Errorf("error = %q, want the ssh message", h.Error)
	}
}

// TestAbsentDataIsOmitted keeps the document free of zeros that read as real
// measurements. A host with no swap must not claim 0 bytes of swap in use.
func TestAbsentDataIsOmitted(t *testing.T) {
	s := model.Sample{At: time.Unix(100, 0)} // a host that reported almost nothing
	f := fleetWith(t, model.HostRef{Name: "bare", Addr: "bare"}, s)
	body := mustJSON(t, Build(f, time.Unix(200, 0)))

	for _, absent := range []string{
		"uptime_seconds", "cores", "cpu_pct",
		"mem_total_bytes", "swap_total_bytes", "swap_used_bytes",
		"filesystems", "network", "disk_io", "temperatures",
		"services", "containers", "failed_units", "reboot_required",
	} {
		if strings.Contains(body, absent) {
			t.Errorf("unreported field %q present in output:\n%s", absent, body)
		}
	}
	// The fields that always mean something must still be there.
	for _, present := range []string{"name", "addr", "status", "healthy"} {
		if !strings.Contains(body, present) {
			t.Errorf("field %q missing from output:\n%s", present, body)
		}
	}
}

func TestServicesAndContainersSerialised(t *testing.T) {
	s := healthySample(time.Unix(100, 0))
	s.Services = []model.Service{
		{Name: "docker", LoadState: "loaded", ActiveState: "active"},
	}
	s.Containers = []model.Container{
		{Runtime: "docker", Name: "matrix-caddy-1", State: "running"},
	}
	f := fleetWith(t, model.HostRef{Name: "nas", Addr: "nas"}, s)

	var doc Fleet
	if err := json.Unmarshal([]byte(mustJSON(t, Build(f, time.Unix(200, 0)))), &doc); err != nil {
		t.Fatalf("output does not round-trip: %v", err)
	}
	h := doc.Hosts[0]

	if len(h.Services) != 1 || h.Services[0].ActiveState != "active" ||
		h.Services[0].LoadState != "loaded" {
		t.Errorf("services = %+v", h.Services)
	}
	if len(h.Containers) != 1 || h.Containers[0].Runtime != "docker" ||
		h.Containers[0].Name != "matrix-caddy-1" {
		t.Errorf("containers = %+v", h.Containers)
	}
}

func TestBuildPreservesHostOrder(t *testing.T) {
	// Config order is the fleet's order, and the report must not reshuffle it.
	f := model.NewFleet([]model.HostRef{
		{Name: "gamma", Addr: "g"}, {Name: "alpha", Addr: "a"}, {Name: "beta", Addr: "b"},
	})
	got := Build(f, time.Unix(200, 0))
	want := []string{"gamma", "alpha", "beta"}
	for i, w := range want {
		if got.Hosts[i].Name != w {
			t.Errorf("host %d = %q, want %q", i, got.Hosts[i].Name, w)
		}
	}
}

func TestWriteProducesIndentedValidJSON(t *testing.T) {
	f := fleetWith(t, model.HostRef{Name: "nas", Addr: "nas"}, healthySample(time.Unix(100, 0)))

	var buf bytes.Buffer
	if err := Write(&buf, Build(f, time.Unix(200, 0))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	var doc Fleet
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	// Indented so it stays readable by eye, not just by jq.
	if !strings.Contains(buf.String(), "\n  ") {
		t.Errorf("output is not indented:\n%s", buf.String())
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("output does not end with a newline")
	}
}

func TestEmptyFleetProducesValidDocument(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Build(model.NewFleet(nil), time.Unix(200, 0))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	var doc Fleet
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("empty fleet produced invalid JSON: %v\n%s", err, buf.String())
	}
	// An empty array rather than null, so `.hosts[]` works without a guard.
	if !strings.Contains(buf.String(), `"hosts": []`) {
		t.Errorf("empty fleet should serialise hosts as [], got:\n%s", buf.String())
	}
}

func mustJSON(t *testing.T, f Fleet) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, f); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	return buf.String()
}

func kind(k model.FailKind) *model.FailKind { return &k }
