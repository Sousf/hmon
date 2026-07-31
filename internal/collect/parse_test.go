package collect

import (
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseFullSample(t *testing.T) {
	out := `v 1
uptime 1209384.12
cpu 4881234 812 1190432 88123945 4412 0 9921 3
mem 32768000 24010448
load 0.42 0.38 0.31
fs / 480000000 190000000 290000000
fs /mnt/tank 8000000000 6200000000 1800000000
net eth0 8402934112 2103928004
temp 52.0 Package id 0
temp 41.5 nvme
end
`
	s, err := Parse([]byte(out), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if !s.HasCPU {
		t.Error("HasCPU = false, want true")
	}
	if got, want := s.CPU.User, uint64(4881234); got != want {
		t.Errorf("CPU.User = %d, want %d", got, want)
	}
	if got, want := s.CPU.Steal, uint64(3); got != want {
		t.Errorf("CPU.Steal = %d, want %d", got, want)
	}

	// Memory arrives in KB and is scaled to bytes on this side.
	if got, want := s.MemTotal, uint64(32768000*1024); got != want {
		t.Errorf("MemTotal = %d, want %d", got, want)
	}

	if got, want := s.Uptime, time.Duration(1209384.12*float64(time.Second)); got != want {
		t.Errorf("Uptime = %v, want %v", got, want)
	}
	if got, want := s.Load[0], 0.42; got != want {
		t.Errorf("Load[0] = %v, want %v", got, want)
	}

	if got, want := len(s.FS), 2; got != want {
		t.Fatalf("FS count = %d, want %d", got, want)
	}
	// UsedPct must match df's Use%: used/(used+avail), excluding reserved
	// blocks, rather than used/total. Using total here would give 39.6% instead
	// of the 39.58% df reports.
	if got, want := s.FS[0].UsedPct(), 39.5833; math.Abs(got-want) > 0.001 {
		t.Errorf("FS[0].UsedPct() = %v, want ~%v", got, want)
	}

	if got, want := len(s.NICs), 1; got != want {
		t.Fatalf("NIC count = %d, want %d", got, want)
	}
	if got, want := s.NICs[0].RxBytes, uint64(8402934112); got != want {
		t.Errorf("RxBytes = %d, want %d", got, want)
	}

	// A sensor label containing spaces must survive intact, which is why the
	// label is the terminal field.
	if got, want := len(s.Temps), 2; got != want {
		t.Fatalf("Temp count = %d, want %d", got, want)
	}
	if got, want := s.Temps[0].Label, "Package id 0"; got != want {
		t.Errorf("Temps[0].Label = %q, want %q", got, want)
	}
	if got, want := s.Temps[0].C, 52.0; got != want {
		t.Errorf("Temps[0].C = %v, want %v", got, want)
	}

	hottest, ok := s.MaxTemp()
	if !ok || hottest.C != 52.0 {
		t.Errorf("MaxTemp() = %v, %v, want 52.0, true", hottest.C, ok)
	}
}

func TestParseRejectsBadOutput(t *testing.T) {
	tests := []struct {
		name string
		out  string
	}{
		{"no version line", "uptime 100\nend\n"},
		{"wrong version", "v 2\nuptime 100\nend\n"},
		// The trailing sentinel is what distinguishes a complete sample from one
		// cut off mid-write by a dropped connection.
		{"truncated", "v 1\nuptime 100\nfs / 100 50 50\n"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.out), time.Unix(0, 0))
			if !errors.Is(err, ErrBadOutput) {
				t.Errorf("Parse() error = %v, want ErrBadOutput", err)
			}
		})
	}
}

func TestParseSkipsMalformedLinesIndividually(t *testing.T) {
	// One unreadable sensor must not discard an otherwise good sample.
	out := `v 1
temp not-a-number broken
temp 41.5 nvme
fs / bogus bogus bogus
fs /data 100 40 60
unknown_key whatever 1 2 3
end
`
	s, err := Parse([]byte(out), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := len(s.Temps), 1; got != want {
		t.Errorf("Temps = %d, want %d (bad line should be skipped, not fatal)", got, want)
	}
	if got, want := len(s.FS), 1; got != want {
		t.Errorf("FS = %d, want %d", got, want)
	}
}

// TestParseRejectsImpossibleTemperatures covers readings seen on real
// hardware: ACPI reports -273.1 (absolute zero) for a sensor that exists but
// is not actually measuring anything.
func TestParseRejectsImpossibleTemperatures(t *testing.T) {
	out := `v 1
temp -273.1 SEN1
temp 31.1 B0D4
temp 21.0 Package id 0
temp 9999 BogusHigh
end
`
	s, err := Parse([]byte(out), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := len(s.Temps), 2; got != want {
		t.Fatalf("Temps = %d, want %d: %+v", got, want, s.Temps)
	}
	for _, tc := range s.Temps {
		if tc.C < -50 || tc.C > 200 {
			t.Errorf("implausible temperature survived: %+v", tc)
		}
	}
	// Labels containing spaces must still come through intact.
	found := false
	for _, tc := range s.Temps {
		if tc.Label == "Package id 0" {
			found = true
		}
	}
	if !found {
		t.Errorf("multi-word sensor label lost: %+v", s.Temps)
	}
}

func TestParseHostWithoutSensors(t *testing.T) {
	// No exposed sensors is normal hardware variation, not an error.
	s, err := Parse([]byte("v 1\nmem 1000 500\nend\n"), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if _, ok := s.MaxTemp(); ok {
		t.Error("MaxTemp() ok = true, want false when no sensors reported")
	}
	if !s.HasMem {
		t.Error("HasMem = false, want true")
	}
}

func TestParseProcsDedupedAndSpacesPreserved(t *testing.T) {
	// The collector sends the union of the CPU and memory rankings, so
	// processes high in both arrive twice.
	out := `v 1
proc 1423 38.2 4300000 qemu-system-x86_64 -name guest=vm1
proc 2891 12.7 913408 postgres
proc 1423 38.2 4300000 qemu-system-x86_64 -name guest=vm1
proc 77 0.0 2048 kworker/0:1H-events_highpri
end
`
	s, err := Parse([]byte(out), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := len(s.Procs), 3; got != want {
		t.Fatalf("Procs = %d, want %d (duplicates by PID should collapse)", got, want)
	}
	// Command is the terminal field, so embedded spaces survive verbatim.
	if got, want := s.Procs[0].Command, "qemu-system-x86_64 -name guest=vm1"; got != want {
		t.Errorf("Procs[0].Command = %q, want %q", got, want)
	}
	// And underscores are not mangled, which a space-escaping scheme would do.
	if got, want := s.Procs[2].Command, "kworker/0:1H-events_highpri"; got != want {
		t.Errorf("Procs[2].Command = %q, want %q", got, want)
	}
	if got, want := s.Procs[0].CPUPct, 38.2; got != want {
		t.Errorf("Procs[0].CPUPct = %v, want %v", got, want)
	}
}

// TestParseRealCollectorOutput runs against output captured from actual Linux
// containers, which is what catches awk portability problems that hand-written
// fixtures would not — mawk rendering large numbers in scientific notation,
// for one.
func TestParseRealCollectorOutput(t *testing.T) {
	files, err := os.ReadDir("testdata")
	if err != nil {
		t.Skipf("no testdata: %v", err)
	}
	found := 0
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".txt") {
			continue
		}
		found++
		t.Run(f.Name(), func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + f.Name())
			if err != nil {
				t.Fatal(err)
			}
			s, err := Parse(data, time.Unix(0, 0))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if !s.HasCPU {
				t.Error("HasCPU = false, want true")
			}
			if !s.HasMem {
				t.Error("HasMem = false, want true — mawk scientific notation regression?")
			}
			if s.MemTotal == 0 {
				t.Error("MemTotal = 0")
			}
			if len(s.NICs) == 0 {
				t.Error("no NICs parsed — /proc/net/dev column alignment regression?")
			}
		})
	}
	if found == 0 {
		t.Skip("no fixtures captured")
	}
}
