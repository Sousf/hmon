package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/Sousf/hmon/internal/config"
	"github.com/Sousf/hmon/internal/model"
)

func init() {
	// Render without ANSI so assertions compare plain text rather than escape
	// sequences.
	lipgloss.SetColorProfile(termenv.Ascii)
}

// fakePoller stands in for SSH. Its existence is the reason no test here needs
// a network or a live host.
type fakePoller struct {
	sample model.Sample
	err    error
	calls  []bool // withProcs for each call
}

func (f *fakePoller) Poll(_ context.Context, _ string, withProcs bool) (model.Sample, error) {
	f.calls = append(f.calls, withProcs)
	if f.err != nil {
		return model.Sample{}, f.err
	}
	return f.sample, nil
}

func testModel(t *testing.T, names ...string) (Model, *model.Fleet) {
	t.Helper()
	if len(names) == 0 {
		names = []string{"alpha", "beta", "gamma"}
	}
	refs := make([]model.HostRef, 0, len(names))
	for _, n := range names {
		refs = append(refs, model.HostRef{Name: n, Addr: n})
	}
	fleet := model.NewFleet(refs)

	cfg, err := config.Parse([]byte("hosts: [x]\n"))
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, fleet, &fakePoller{}), fleet
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// send applies a sequence of messages, which is all Update is: a pure function
// from (state, message) to state.
func send(m Model, msgs ...tea.Msg) Model {
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(Model)
	}
	return m
}

func TestMoveClampsAtEnds(t *testing.T) {
	m, _ := testModel(t)
	if got, want := m.selected, "alpha"; got != want {
		t.Fatalf("initial selection = %q, want %q", got, want)
	}

	m = send(m, key("up")) // already at top
	if got, want := m.selected, "alpha"; got != want {
		t.Errorf("after up at top = %q, want %q (must clamp, not wrap)", got, want)
	}

	m = send(m, key("down"), key("down"), key("down"), key("down"))
	if got, want := m.selected, "gamma"; got != want {
		t.Errorf("after repeated down = %q, want %q (must clamp at bottom)", got, want)
	}
}

// TestSelectionSurvivesReordering is the reason selection is tracked by name
// rather than index: re-sorting must not silently move the cursor to a
// different machine.
func TestSelectionSurvivesReordering(t *testing.T) {
	m, fleet := testModel(t)
	fleet.Apply("alpha", Sample(t, 10))
	fleet.Apply("beta", Sample(t, 90))
	fleet.Apply("gamma", Sample(t, 50))
	// Second samples so CPU percentages exist.
	fleet.Apply("alpha", Sample2(t, 10))
	fleet.Apply("beta", Sample2(t, 90))
	fleet.Apply("gamma", Sample2(t, 50))

	m = send(m, key("down")) // select beta
	if got, want := m.selected, "beta"; got != want {
		t.Fatalf("selection = %q, want %q", got, want)
	}

	// Switch to CPU sort, which reorders the table.
	m = send(m, key("s"))
	for m.sort != sortCPU {
		m = send(m, key("s"))
	}
	if got, want := m.selected, "beta"; got != want {
		t.Errorf("selection after re-sort = %q, want %q", got, want)
	}
}

func TestSortByCPUDescending(t *testing.T) {
	m, fleet := testModel(t)
	for _, tc := range []struct {
		name string
		cpu  float64
	}{{"alpha", 10}, {"beta", 90}, {"gamma", 50}} {
		fleet.Apply(tc.name, Sample(t, tc.cpu))
		fleet.Apply(tc.name, Sample2(t, tc.cpu))
	}

	m.sort = sortCPU
	m.sortDesc = true
	got := hostNames(m.sortedHosts())
	want := []string{"beta", "gamma", "alpha"}
	if !equal(got, want) {
		t.Errorf("sorted = %v, want %v", got, want)
	}

	m.sortDesc = false
	got = hostNames(m.sortedHosts())
	want = []string{"alpha", "gamma", "beta"}
	if !equal(got, want) {
		t.Errorf("inverted = %v, want %v", got, want)
	}
}

func TestSortIsStableForEqualValues(t *testing.T) {
	// Every host idle at 0% must not reshuffle between frames.
	m, fleet := testModel(t)
	for _, n := range []string{"alpha", "beta", "gamma"} {
		fleet.Apply(n, Sample(t, 0))
		fleet.Apply(n, Sample2(t, 0))
	}
	m.sort = sortCPU
	m.sortDesc = true

	first := hostNames(m.sortedHosts())
	for i := 0; i < 5; i++ {
		if got := hostNames(m.sortedHosts()); !equal(got, first) {
			t.Fatalf("order changed between renders: %v then %v", first, got)
		}
	}
}

func TestSampleAndFailMessagesUpdateFleet(t *testing.T) {
	m, fleet := testModel(t)

	m = send(m, sampleMsg{host: "alpha", sample: Sample(t, 10)})
	h, _ := fleet.Get("alpha")
	if got, want := h.Status, model.StatusUp; got != want {
		t.Errorf("Status = %v, want %v", got, want)
	}

	m = send(m,
		failMsg{host: "alpha", kind: model.FailUnreachable, err: "timeout"},
		failMsg{host: "alpha", kind: model.FailUnreachable, err: "timeout"},
	)
	if got, want := h.Status, model.StatusDown; got != want {
		t.Errorf("Status after 2 failures = %v, want %v", got, want)
	}
	if got, want := h.LastErr, "timeout"; got != want {
		t.Errorf("LastErr = %q, want %q", got, want)
	}
	_ = m
}

func TestInFlightClearedOnResult(t *testing.T) {
	m, _ := testModel(t)
	m.inFlight["alpha"] = true
	m = send(m, sampleMsg{host: "alpha", sample: Sample(t, 10)})
	if m.inFlight["alpha"] {
		t.Error("inFlight still set after sample, want cleared")
	}

	m.inFlight["beta"] = true
	m = send(m, failMsg{host: "beta", kind: model.FailUnreachable, err: "x"})
	if m.inFlight["beta"] {
		t.Error("inFlight still set after failure, want cleared")
	}
}

// TestProcsOnlyRequestedForDetailHost guards the decision that process
// collection — which costs an extra sampling window remotely — never runs
// fleet-wide.
func TestProcsOnlyRequestedForDetailHost(t *testing.T) {
	m, _ := testModel(t)
	fake := &fakePoller{}
	m.poller = fake

	// Table view: no host should be asked for processes.
	cmd := m.pollAll()
	drain(cmd)
	for i, withProcs := range fake.calls {
		if withProcs {
			t.Errorf("call %d requested procs in table view, want none", i)
		}
	}

	// Detail view: only the selected host.
	fake.calls = nil
	m.view = viewDetail
	m.selected = "beta"
	cmd = m.pollAll()
	drain(cmd)
	count := 0
	for _, withProcs := range fake.calls {
		if withProcs {
			count++
		}
	}
	if got, want := count, 1; got != want {
		t.Errorf("procs requested for %d hosts, want %d", got, want)
	}
}

func TestEnterOpensDetailAndEscReturns(t *testing.T) {
	m, _ := testModel(t)
	m.poller = &fakePoller{}

	m = send(m, key("enter"))
	if m.view != viewDetail {
		t.Error("enter did not open detail view")
	}
	m = send(m, key("esc"))
	if m.view != viewTable {
		t.Error("esc did not return to table view")
	}
}

func TestProcSortKeysOnlyApplyInDetail(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, key("m"))
	if m.procSort != procByCPU {
		t.Error("m changed proc sort from the table view, want ignored")
	}

	m.view = viewDetail
	m = send(m, key("m"))
	if m.procSort != procByMem {
		t.Error("m did not change proc sort in detail view")
	}
	m = send(m, key("c"))
	if m.procSort != procByCPU {
		t.Error("c did not change proc sort back to cpu")
	}
}

func TestQuitSetsQuittingAndBlanksView(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, key("q"))
	if !m.quitting {
		t.Error("quitting = false after q")
	}
	if got := m.View(); got != "" {
		t.Errorf("View() = %q while quitting, want empty", got)
	}
}

func TestRenderTableShowsHostsAndStatus(t *testing.T) {
	m, fleet := testModel(t)
	fleet.Apply("alpha", Sample(t, 10))
	fleet.Apply("alpha", Sample2(t, 10))
	fleet.Fail("beta", model.FailUnreachable, "timeout")
	fleet.Fail("beta", model.FailUnreachable, "timeout")
	fleet.Fail("gamma", model.FailAuth, "permission denied")

	out := m.View()
	for _, want := range []string{"alpha", "beta", "gamma", "up", "down", "auth"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n---\n%s", want, out)
		}
	}
	// A down host must not display stale readings as if current.
	lines := strings.Split(out, "\n")
	for _, ln := range lines {
		if strings.Contains(ln, "beta") && strings.Contains(ln, "%") {
			t.Errorf("down host shows a percentage: %q", ln)
		}
	}
}

// TestSparklineIsBlankWhenIdle guards against an idle fleet drawing eight ▁ in
// a row, which merges into a solid rule and reads as a table border rather
// than as data.
func TestSparklineIsBlankWhenIdle(t *testing.T) {
	idle := []float64{1, 0.4, 1.2, 0, 1, 0.8, 1, 1}
	if got := sparkline(idle, 8, 100); strings.TrimSpace(got) != "" {
		t.Errorf("idle sparkline = %q, want all blank", got)
	}

	// Real activity must still render, otherwise the column is useless.
	busy := []float64{5, 20, 45, 80, 95, 60, 30, 10}
	got := sparkline(busy, 8, 100)
	if strings.TrimSpace(got) == "" {
		t.Errorf("busy sparkline = %q, want visible blocks", got)
	}
	if !strings.ContainsRune(got, '█') {
		t.Errorf("busy sparkline = %q, want a full block for the 95%% sample", got)
	}

	// Width must stay fixed either way, or columns to the right shift.
	if a, b := len([]rune(sparkline(idle, 8, 100))), len([]rune(got)); a != b || a != 8 {
		t.Errorf("sparkline widths = %d and %d, want both 8", a, b)
	}
}

// TestTableHeaderAlignsWithRows guards a bug the unit tests could not see:
// data rows carry a two-character selection cursor, so a header without the
// same pad puts every column label two characters left of its values.
func TestTableHeaderAlignsWithRows(t *testing.T) {
	m, fleet := testModel(t, "alpha", "beta")
	fleet.Apply("alpha", Sample(t, 10))
	fleet.Apply("alpha", Sample2(t, 10))

	lines := strings.Split(m.View(), "\n")
	var header, row string
	for i, ln := range lines {
		if strings.Contains(ln, "HOST") && strings.Contains(ln, "STATUS") {
			header = ln
			// The first data row follows immediately.
			if i+1 < len(lines) {
				row = lines[i+1]
			}
			break
		}
	}
	if header == "" || row == "" {
		t.Fatalf("could not locate header and first row in:\n%s", m.View())
	}

	// Compare display columns, not byte offsets: the cursor glyph ▸ is three
	// bytes wide but occupies one column.
	if got, want := colOf(header, "HOST"), colOf(row, "alpha"); got != want {
		t.Errorf("HOST header at column %d but host name at column %d", got, want)
	}
	if got, want := colOf(header, "STATUS"), colOf(row, "●"); got != want {
		t.Errorf("STATUS header at column %d but status dot at column %d", got, want)
	}
}

// colOf returns the display column where sub begins in s, counting runes
// rather than bytes.
func colOf(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return len([]rune(s[:i]))
}

func TestRenderDetailShowsErrorForDownHost(t *testing.T) {
	m, fleet := testModel(t)
	fleet.Fail("alpha", model.FailAuth, "Permission denied (publickey)")

	m.view = viewDetail
	m.selected = "alpha"
	out := m.View()
	// Diagnosing a red row should not require leaving the tool.
	if !strings.Contains(out, "Permission denied") {
		t.Errorf("detail view missing error text\n---\n%s", out)
	}
}

func TestRenderDetailShowsProcesses(t *testing.T) {
	m, fleet := testModel(t)
	s := Sample(t, 10)
	s.Procs = []model.Proc{
		{PID: 1423, CPUPct: 38.2, RSSKB: 4300000, Command: "qemu-system-x86_64"},
		{PID: 2891, CPUPct: 12.7, RSSKB: 913408, Command: "postgres"},
	}
	fleet.Apply("alpha", s)

	m.view = viewDetail
	m.selected = "alpha"
	out := m.View()
	for _, want := range []string{"TOP PROCESSES", "qemu-system-x86_64", "postgres", "1423"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail output missing %q\n---\n%s", want, out)
		}
	}
}

func TestDetailProcSortByMemory(t *testing.T) {
	m, fleet := testModel(t)
	s := Sample(t, 10)
	// Highest CPU is not the highest memory, so the orderings differ.
	s.Procs = []model.Proc{
		{PID: 1, CPUPct: 90, RSSKB: 1000, Command: "busy-small"},
		{PID: 2, CPUPct: 1, RSSKB: 9000000, Command: "idle-huge"},
	}
	fleet.Apply("alpha", s)

	m.view = viewDetail
	m.selected = "alpha"

	m.procSort = procByCPU
	if got := firstProcLine(m.View()); !strings.Contains(got, "busy-small") {
		t.Errorf("cpu sort put %q first, want busy-small", got)
	}

	m.procSort = procByMem
	if got := firstProcLine(m.View()); !strings.Contains(got, "idle-huge") {
		t.Errorf("mem sort put %q first, want idle-huge", got)
	}
}

func TestPollErrorBecomesFailMsgWithClassifiedKind(t *testing.T) {
	m, _ := testModel(t)
	m.poller = &fakePoller{err: errors.New("connection refused")}

	msg := m.pollOne("alpha", "alpha", false)()
	fm, ok := msg.(failMsg)
	if !ok {
		t.Fatalf("got %T, want failMsg", msg)
	}
	if fm.kind != model.FailUnreachable {
		t.Errorf("kind = %v, want FailUnreachable", fm.kind)
	}
}

// --- helpers ---------------------------------------------------------------

// Sample builds a first sample whose CPU counters put the host at the given
// percentage once a second sample arrives.
func Sample(t *testing.T, cpuPct float64) model.Sample {
	t.Helper()
	return model.Sample{
		At:       time.Unix(100, 0),
		HasCPU:   true,
		HasMem:   true,
		CPU:      model.CPUTimes{User: 0, Idle: 0},
		MemTotal: 1000,
		MemAvail: 500,
		FS:       []model.FS{{Mount: "/", TotalKB: 100, UsedKB: 40, AvailKB: 60}},
		NICs:     []model.NIC{{Name: "eth0", RxBytes: 0, TxBytes: 0}},
		Temps:    []model.Temp{{Label: "cpu", C: 45}},
	}
}

// Sample2 is the follow-up sample two seconds later, chosen so the busy/total
// jiffy ratio equals cpuPct.
func Sample2(t *testing.T, cpuPct float64) model.Sample {
	t.Helper()
	const total = 1000
	busy := uint64(cpuPct / 100 * total)
	return model.Sample{
		At:       time.Unix(102, 0),
		HasCPU:   true,
		HasMem:   true,
		CPU:      model.CPUTimes{User: busy, Idle: total - busy},
		MemTotal: 1000,
		MemAvail: 500,
		FS:       []model.FS{{Mount: "/", TotalKB: 100, UsedKB: 40, AvailKB: 60}},
		NICs:     []model.NIC{{Name: "eth0", RxBytes: 2000, TxBytes: 1000}},
		Temps:    []model.Temp{{Label: "cpu", C: 45}},
	}
}

func hostNames(hosts []*model.Host) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// drain executes a tea.Cmd (and any batched children) for its side effects.
func drain(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			drain(c)
		}
	}
}

// firstProcLine returns the first process row after the header.
func firstProcLine(out string) string {
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "PID") && strings.Contains(ln, "COMMAND") {
			if i+1 < len(lines) {
				return lines[i+1]
			}
		}
	}
	return ""
}
