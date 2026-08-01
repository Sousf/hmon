package ui

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"

	"github.com/Sousf/hmon/internal/collect"
	"github.com/Sousf/hmon/internal/config"
	"github.com/Sousf/hmon/internal/model"
)

func init() {
	// Render without ANSI so assertions compare plain text rather than escape
	// sequences.
	lipgloss.SetColorProfile(termenv.Ascii)
}

// fakeExecutor stands in for ad-hoc command execution, recording what it was
// asked to run so tests can assert on scope without touching SSH.
type fakeExecutor struct {
	// Guarded because a fan-out calls Exec from one goroutine per host, which
	// is the whole point of the feature under test.
	mu       sync.Mutex
	script   string
	asRoot   bool
	password string
	addrs    []string
	result   collect.ExecResult
	err      error
}

func (f *fakeExecutor) Exec(_ context.Context, addr string, req collect.ExecRequest) (collect.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = req.Script
	f.asRoot = req.AsRoot
	f.password = req.Password
	f.addrs = append(f.addrs, addr)
	return f.result, f.err
}

func (f *fakeExecutor) lastRequest() (script string, asRoot bool, password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.script, f.asRoot, f.password
}

func (f *fakeExecutor) lastScript() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.script
}

// fakePoller stands in for SSH. Its existence is the reason no test here needs
// a network or a live host.
type fakePoller struct {
	sample   model.Sample
	err      error
	calls    []bool // opts.Detail for each call
	opts     []collect.Opts
	services []string
}

func (f *fakePoller) Poll(_ context.Context, _ string, opts collect.Opts) (model.Sample, error) {
	f.calls = append(f.calls, opts.Detail)
	f.opts = append(f.opts, opts)
	f.services = opts.Services
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
	return New(cfg, fleet, &fakePoller{}, &fakeExecutor{}), fleet
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
	// A healthy host is a bare green dot with no word; the states that need
	// explaining are the only ones carrying text.
	for _, want := range []string{"alpha", "beta", "gamma", "●", "down", "auth"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q\n---\n%s", want, out)
		}
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "alpha") && strings.Contains(ln, " up") {
			t.Errorf("healthy host still labelled: %q", ln)
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

// TestFailedUnitFlaggedInTable covers the gap the health check exists to
// close: a host with a crashed service looks perfectly healthy in every
// resource column, so the failure has to be visible without drilling in.
func TestFailedUnitFlaggedInTable(t *testing.T) {
	m, fleet := testModel(t)
	s := Sample(t, 10)
	s.FailedUnits = []string{"postgresql.service"}
	s.HasUnitInfo = true
	fleet.Apply("alpha", s)

	healthy := Sample(t, 10)
	healthy.HasUnitInfo = true
	fleet.Apply("beta", healthy)

	out := m.View()
	var alphaLine, betaLine string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "alpha") {
			alphaLine = ln
		}
		if strings.Contains(ln, "beta") {
			betaLine = ln
		}
	}
	if !strings.Contains(alphaLine, "✗") {
		t.Errorf("host with a failed unit not flagged: %q", alphaLine)
	}
	if strings.Contains(betaLine, "✗") {
		t.Errorf("healthy host wrongly flagged: %q", betaLine)
	}
}

func TestRebootRequiredFlagged(t *testing.T) {
	m, fleet := testModel(t)
	s := Sample(t, 10)
	s.RebootRequired = true
	fleet.Apply("alpha", s)

	if !strings.Contains(m.View(), glyphReboot) {
		t.Error("reboot-required host not flagged in table")
	}
}

// TestDetailPaneShowsHealthAndCapacity checks the pane surfaces what the table
// only hints at.
func TestDetailPaneShowsHealthAndCapacity(t *testing.T) {
	m, fleet := testModel(t)
	s := Sample(t, 10)
	s.FailedUnits = []string{"openipmi.service"}
	s.HasUnitInfo = true
	s.RebootRequired = true
	s.Cores = 16
	s.Load = [3]float64{8, 4, 2}
	s.SwapTotal = 4 << 30
	s.SwapFree = 1 << 30
	fleet.Apply("alpha", s)
	h, _ := fleet.Get("alpha")

	out := strings.Join(m.detailPane(h, 40), "\n")
	for _, want := range []string{
		"openipmi.service", // the actual broken unit, named
		"reboot required",
		"16 cores", // load is meaningless without this
		"SWAP",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail pane missing %q\n---\n%s", want, out)
		}
	}
}

func TestSwapLineHiddenWhenNoSwapConfigured(t *testing.T) {
	m, fleet := testModel(t)
	fleet.Apply("alpha", Sample(t, 10)) // no swap fields set
	h, _ := fleet.Get("alpha")

	if out := strings.Join(m.detailPane(h, 40), "\n"); strings.Contains(out, "SWAP") {
		t.Errorf("swap line shown for a host with no swap:\n%s", out)
	}
}

// TestRebootRequiresConfirmation is the important one: R must never reach a
// machine on its own. It is the only action in hmon that changes a host rather
// than reading it.
func TestRebootRequiresConfirmation(t *testing.T) {
	m, _ := testModel(t)
	m.selected = "beta"

	next, cmd := m.Update(key("R"))
	m = next.(Model)
	if cmd != nil {
		t.Fatal("R issued a command before confirmation — must only open the dialog")
	}
	if m.confirmReboot != "beta" {
		t.Errorf("confirmReboot = %q, want beta", m.confirmReboot)
	}

	// The dialog must name the host and show the exact command.
	out := m.View()
	for _, want := range []string{"Reboot beta?", "systemctl reboot", "y", "cancel"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirm dialog missing %q\n---\n%s", want, out)
		}
	}
}

func TestRebootConfirmedOnlyByY(t *testing.T) {
	m, _ := testModel(t)

	// Anything other than y cancels, so mashing cannot confirm.
	for _, k := range []string{"n", "N", "esc", "enter", "q", "r", "R", " "} {
		mm := m
		mm.confirmReboot = "alpha"
		next, cmd := mm.Update(key(k))
		got := next.(Model)
		if cmd != nil {
			t.Errorf("key %q issued a command, want cancel", k)
		}
		if got.confirmReboot != "" {
			t.Errorf("key %q left the dialog open", k)
		}
	}

	m.confirmReboot = "alpha"
	next, cmd := m.Update(key("y"))
	got := next.(Model)
	if cmd == nil {
		t.Error("y did not issue the reboot command")
	}
	if got.confirmReboot != "" {
		t.Error("dialog still open after confirming")
	}
}

// TestConfirmDialogSwallowsOtherBindings guards against a stray keypress doing
// something else entirely while the dialog is up — quitting, re-sorting, or
// opening an ssh session behind the prompt.
func TestConfirmDialogSwallowsOtherBindings(t *testing.T) {
	m, _ := testModel(t)
	m.confirmReboot = "alpha"
	m.sort = sortName

	next, _ := m.Update(key("s"))
	got := next.(Model)
	if got.sort != sortName {
		t.Error("sort changed while the confirm dialog was showing")
	}
	if got.quitting {
		t.Error("model quit while the confirm dialog was showing")
	}
}

func TestConfirmDialogWarnsWhenHostIsNotUp(t *testing.T) {
	// Rebooting something already unreachable cannot work, and usually means
	// the wrong row is selected.
	m, fleet := testModel(t)
	fleet.Fail("alpha", model.FailUnreachable, "timeout")
	fleet.Fail("alpha", model.FailUnreachable, "timeout")

	m.confirmReboot = "alpha"
	if out := m.View(); !strings.Contains(out, "currently down") {
		t.Errorf("dialog does not warn that the host is down\n---\n%s", out)
	}
}

func TestSSHKeyLaunchesSessionForSelectedHost(t *testing.T) {
	m, _ := testModel(t)
	m.selected = "beta"

	_, cmd := m.Update(key("S"))
	if cmd == nil {
		t.Fatal("S produced no command, want an ssh exec")
	}
	// The process is not actually run here — that would take over the test's
	// terminal — but the command must exist and the model must not have
	// changed view or selection.
	if m.view != viewTable || m.selected != "beta" {
		t.Errorf("S changed state: view=%v selected=%q", m.view, m.selected)
	}
}

func TestSplitActivatesOnlyWhenTallEnough(t *testing.T) {
	m, _ := testModel(t)

	// Height is zero until the first WindowSizeMsg, so the first frame must
	// stay compact rather than guessing.
	if m.splitActive() {
		t.Error("split active with unknown height, want compact")
	}

	m = send(m, tea.WindowSizeMsg{Width: 100, Height: 12})
	if m.splitActive() {
		t.Error("split active on a short terminal, want compact")
	}

	m = send(m, tea.WindowSizeMsg{Width: 100, Height: 44})
	if !m.splitActive() {
		t.Error("split inactive on a tall terminal, want the detail pane")
	}

	// The full detail view owns the whole screen; no pane underneath a table
	// that is not being drawn.
	m.view = viewDetail
	if m.splitActive() {
		t.Error("split active while in full detail view")
	}
}

// TestDetailPaneRespectsBudget guards the layout invariant: the pane must
// never render more lines than it was given, or it pushes the help line off
// the bottom of the screen.
func TestDetailPaneRespectsBudget(t *testing.T) {
	m, fleet := testModel(t)
	s := Sample(t, 10)
	s.FS = []model.FS{
		{Mount: "/", TotalKB: 100, UsedKB: 40, AvailKB: 60},
		{Mount: "/boot", TotalKB: 100, UsedKB: 10, AvailKB: 90},
		{Mount: "/mnt/tank", TotalKB: 100, UsedKB: 80, AvailKB: 20},
	}
	for i := 0; i < 40; i++ {
		s.Procs = append(s.Procs, model.Proc{
			PID: i + 1, CPUPct: float64(i), RSSKB: uint64(i) * 1000, Command: "proc",
		})
	}
	fleet.Apply("alpha", s)
	h, _ := fleet.Get("alpha")

	for _, budget := range []int{1, 5, 8, 12, 20, 50} {
		got := m.detailPane(h, budget)
		if len(got) > budget {
			t.Errorf("budget %d produced %d lines, want at most %d", budget, len(got), budget)
		}
	}
	if got := m.detailPane(h, 0); got != nil {
		t.Errorf("zero budget produced %d lines, want none", len(got))
	}
}

func TestSplitPaneRequestsProcsForSelectedHostOnly(t *testing.T) {
	m, _ := testModel(t)
	fake := &fakePoller{}
	m.poller = fake
	// Tall terminal, table view: the pane is showing, so its host needs
	// processes even though we never pressed enter.
	m = send(m, tea.WindowSizeMsg{Width: 100, Height: 44})
	m.poller = fake
	m.selected = "beta"

	drain(m.pollAll())
	count := 0
	for _, withProcs := range fake.calls {
		if withProcs {
			count++
		}
	}
	if got, want := count, 1; got != want {
		t.Errorf("procs requested for %d hosts, want %d (selected host only)", got, want)
	}
}

// TestProcSortFallsBackToMemory covers idle hosts, where every process reports
// 0.0% CPU and the ordering would otherwise be whatever the collector emitted
// — filling the list with zero-RSS kernel threads.
func TestProcSortFallsBackToMemory(t *testing.T) {
	procs := []model.Proc{
		{PID: 1, CPUPct: 0, RSSKB: 0, Command: "kworker/1:0"},
		{PID: 2, CPUPct: 0, RSSKB: 109000, Command: "lxd"},
		{PID: 3, CPUPct: 0, RSSKB: 0, Command: "kworker/2:0"},
		{PID: 4, CPUPct: 0, RSSKB: 102000, Command: "dockerd"},
	}
	got := sortedProcs(procs, procByCPU)
	if got[0].Command != "lxd" || got[1].Command != "dockerd" {
		t.Errorf("idle ordering = %s, %s; want lxd, dockerd (largest RSS first)",
			got[0].Command, got[1].Command)
	}

	// A process actually using CPU must still outrank a memory-heavy idle one.
	procs = append(procs, model.Proc{PID: 5, CPUPct: 1.9, RSSKB: 500, Command: "python"})
	got = sortedProcs(procs, procByCPU)
	if got[0].Command != "python" {
		t.Errorf("busy ordering put %s first, want python", got[0].Command)
	}
}

// TestPaddingMeasuresDisplayColumns covers text the terminal draws wider than
// one cell per rune. Counting runes there shifts every column to the right of
// it for that row only, which is the worst kind of layout bug — it looks fine
// until one host has a CJK process name.
func TestPaddingMeasuresDisplayColumns(t *testing.T) {
	wide := "日本語" // three runes, six display columns

	if got := padRight(wide, 10); runewidth.StringWidth(got) != 10 {
		t.Errorf("padRight(%q, 10) has width %d, want 10", wide, runewidth.StringWidth(got))
	}
	if got := padLeft(wide, 10); runewidth.StringWidth(got) != 10 {
		t.Errorf("padLeft(%q, 10) has width %d, want 10", wide, runewidth.StringWidth(got))
	}
	// Already at or over the width: leave it alone rather than padding to a
	// rune count that would overflow the column.
	if got := padRight(wide, 4); got != wide {
		t.Errorf("padRight(%q, 4) = %q, want unchanged", wide, got)
	}
	if got := truncate(wide, 4); runewidth.StringWidth(got) > 4 {
		t.Errorf("truncate(%q, 4) = %q, width %d, want at most 4",
			wide, got, runewidth.StringWidth(got))
	}
}

// TestHealthGlyphsAreSingleColumn guards the fix for a real rendering bug: ⟳
// (U+27F3) measures one column and the terminal advances one cell, but many
// monospace fonts draw it wider than its cell and it bleeds over the following
// character. No width calculation can catch that, so the defence is to keep
// these markers to characters that are safe in any font.
func TestHealthGlyphsAreSingleColumn(t *testing.T) {
	for _, g := range []string{glyphFailed, glyphReboot} {
		if got := runewidth.StringWidth(g); got != 1 {
			t.Errorf("glyph %q measures %d columns, want 1", g, got)
		}
	}
	// The reboot marker in particular must stay ASCII: it is the one that was
	// previously over-drawn, and ASCII cannot be.
	for _, r := range glyphReboot {
		if r > 127 {
			t.Errorf("reboot glyph %q is not ASCII; a font may draw it oversized", glyphReboot)
		}
	}
}

// TestFlaggedAndUnflaggedRowsAlign checks that a host carrying a health marker
// keeps its later columns lined up with a host that has none.
func TestFlaggedAndUnflaggedRowsAlign(t *testing.T) {
	m, fleet := testModel(t, "alpha", "beta", "gamma")
	flagged := Sample(t, 10)
	flagged.FailedUnits = []string{"x.service"}
	flagged.HasUnitInfo = true
	fleet.Apply("alpha", flagged)

	rebooting := Sample(t, 10)
	rebooting.RebootRequired = true
	fleet.Apply("beta", rebooting)

	fleet.Apply("gamma", Sample(t, 10)) // no marker

	var cols []int
	for _, ln := range strings.Split(m.View(), "\n") {
		for _, name := range []string{"alpha", "beta", "gamma"} {
			if strings.Contains(ln, name) && strings.Contains(ln, "●") {
				cols = append(cols, colOf(ln, "●"))
			}
		}
	}
	if len(cols) != 3 {
		t.Fatalf("found %d host rows, want 3", len(cols))
	}
	for i, c := range cols {
		if c != cols[0] {
			t.Errorf("row %d status column at %d, want %d — health marker broke alignment",
				i, c, cols[0])
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

	// The status column is centred rather than left-aligned, so its header and
	// its contents deliberately do not share a starting column. What must hold
	// is that the dot lands inside the column, near its middle.
	statusStart := colOf(header, "STATUS")
	dot := colOf(row, "●")
	centre := statusStart - (colStatusW-len("STATUS"))/2 + colStatusW/2
	if dot < statusStart-2 || dot > statusStart+colStatusW {
		t.Errorf("status dot at column %d is outside the STATUS column starting at %d",
			dot, statusStart)
	}
	if diff := dot - centre; diff < -1 || diff > 1 {
		t.Errorf("status dot at column %d, want within one of the column centre %d",
			dot, centre)
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

	h, _ := m.fleet.Get("alpha")
	msg := m.pollOne(h, false)()
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

// containerAwarePoller mimics the real collector: it only reports containers
// when they were actually requested, which is what made the original bug
// invisible to a fake that always returned them.
type containerAwarePoller struct {
	requested []bool // opts.Containers per call
}

func (p *containerAwarePoller) Poll(_ context.Context, _ string, opts collect.Opts) (model.Sample, error) {
	p.requested = append(p.requested, opts.Containers || opts.Detail)
	s := model.Sample{At: time.Unix(100, 0), HasCPU: true, HasMem: true, MemTotal: 100, MemAvail: 50}
	if opts.Containers || opts.Detail {
		s.HasContainerInfo = true
		s.Containers = []model.Container{{Runtime: "docker", Name: "minecraft", State: "running"}}
	}
	return s, nil
}

// TestWatchedContainersPolledForEveryHost covers the bug where moving the
// selection made other hosts show a health flag: containers were only
// collected for the host being viewed, so every watched container on every
// other host was synthesised as missing.
func TestWatchedContainersPolledForEveryHost(t *testing.T) {
	fleet := model.NewFleet([]model.HostRef{
		{Name: "alpha", Addr: "alpha", Containers: []string{"minecraft"}},
		{Name: "beta", Addr: "beta", Containers: []string{"minecraft"}},
	})
	cfg, err := config.Parse([]byte("hosts: [x]\n"))
	if err != nil {
		t.Fatal(err)
	}

	fake := &containerAwarePoller{}
	m := New(cfg, fleet, fake, &fakeExecutor{})
	m.selected = "alpha"

	drain(m.pollAll())
	if len(fake.requested) != 2 {
		t.Fatalf("polled %d hosts, want 2", len(fake.requested))
	}
	for i, asked := range fake.requested {
		if !asked {
			t.Errorf("host %d not asked for containers despite a watch list", i)
		}
	}

	// Feed both results through, as the real loop would, and confirm the
	// unselected host is not flagged.
	for _, name := range []string{"alpha", "beta"} {
		s, _ := fake.Poll(context.Background(), name, collect.Opts{Containers: true})
		m = send(m, sampleMsg{host: name, sample: s})
	}

	for _, ln := range strings.Split(m.View(), "\n") {
		if strings.Contains(ln, "beta") && strings.Contains(ln, glyphFailed) {
			t.Errorf("unselected host flagged despite a running container: %q", ln)
		}
	}
}

func TestPadCentre(t *testing.T) {
	tests := []struct {
		s     string
		width int
		want  string
	}{
		{"●", 9, "    ●    "},
		{"○ down", 9, " ○ down  "},    // odd remainder goes right
		{"⚠ bad out", 9, "⚠ bad out"}, // exact fit, untouched
		{"toolong", 3, "toolong"},     // never truncates
		{"", 4, "    "},
	}
	for _, tt := range tests {
		if got := padCentre(tt.s, tt.width); got != tt.want {
			t.Errorf("padCentre(%q, %d) = %q, want %q", tt.s, tt.width, got, tt.want)
		}
	}

	// Double-width input must be measured in columns, like the other padders.
	if got := padCentre("日本", 8); runewidth.StringWidth(got) != 8 {
		t.Errorf("padCentre with double-width text has width %d, want 8",
			runewidth.StringWidth(got))
	}
}

// TestStatusDotsAlignAcrossRows is the property that actually matters: with a
// healthy fleet every dot must sit in the same column, so the eye can scan
// straight down them.
func TestStatusDotsAlignAcrossRows(t *testing.T) {
	m, fleet := testModel(t, "a", "bb", "ccc")
	for _, n := range []string{"a", "bb", "ccc"} {
		fleet.Apply(n, Sample(t, 10))
	}

	var cols []int
	for _, ln := range strings.Split(m.View(), "\n") {
		if c := colOf(ln, "●"); c >= 0 {
			cols = append(cols, c)
		}
	}
	if len(cols) != 3 {
		t.Fatalf("found %d status dots, want 3", len(cols))
	}
	for i, c := range cols {
		if c != cols[0] {
			t.Errorf("dot %d at column %d, want %d — hosts of differing name length "+
				"must not shift the status column", i, c, cols[0])
		}
	}
}

// TestCommandDefaultsToSelectedHostNotAllHosts is the safety property that
// matters most here: with nothing marked, a command must reach one machine.
// Defaulting an empty mark set to "everything" is how you accidentally run a
// command on the whole fleet.
func TestCommandDefaultsToSelectedHostNotAllHosts(t *testing.T) {
	m, _ := testModel(t)
	m.selected = "beta"

	got := m.targets()
	if len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("targets = %v, want just beta", hostNames(got))
	}
}

func TestMarkingSelectsHostsForCommand(t *testing.T) {
	m, _ := testModel(t)

	m = send(m, key(" ")) // mark alpha
	m = send(m, key("down"), key(" "))
	if got, want := len(m.targets()), 2; got != want {
		t.Fatalf("targets = %d, want %d", got, want)
	}

	// Marks are independent of the cursor: moving away keeps them.
	m = send(m, key("down"))
	if got := hostNames(m.targets()); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("targets = %v, want [alpha beta]", got)
	}

	// Toggling off restores the single-host default.
	m = send(m, key("up"), key(" "), key("up"), key(" "))
	if got := hostNames(m.targets()); len(got) != 1 {
		t.Errorf("targets = %v, want a single host after unmarking", got)
	}
}

func TestMarkAllToggles(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, key("a"))
	if got, want := len(m.targets()), 3; got != want {
		t.Fatalf("after a: targets = %d, want %d", got, want)
	}
	// Pressing it again clears rather than inverting, so what is marked is
	// knowable without reading the table.
	m = send(m, key("a"))
	if got, want := len(m.marked), 0; got != want {
		t.Errorf("after second a: marked = %d, want %d", got, want)
	}
}

func TestMarkedHostsAreVisibleInTable(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, key(" "))

	var alphaLine, betaLine string
	for _, ln := range strings.Split(m.View(), "\n") {
		if strings.Contains(ln, "alpha") {
			alphaLine = ln
		}
		if strings.Contains(ln, "beta") {
			betaLine = ln
		}
	}
	if !strings.Contains(alphaLine, "*") {
		t.Errorf("marked host carries no mark: %q", alphaLine)
	}
	if strings.Contains(betaLine, "*") {
		t.Errorf("unmarked host shows a mark: %q", betaLine)
	}
}

// TestPromptSwallowsAllKeys guards against a command containing "q" quitting
// the program, or "r" triggering a refresh, while it is being typed.
func TestPromptSwallowsAllKeys(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, key("x"))
	if m.view != viewPrompt {
		t.Fatal("x did not open the prompt")
	}

	for _, k := range []string{"q", "r", "R", "s", "a", "j", "k"} {
		m = send(m, key(k))
	}
	if m.quitting {
		t.Error("typing q into the prompt quit the program")
	}
	if m.view != viewPrompt {
		t.Errorf("view = %v, want the prompt to stay open", m.view)
	}
	if got, want := m.prompt, "qrRsajk"; got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

func TestPromptEditingAndCancel(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, key("x"))
	for _, r := range "df -h" {
		m = send(m, key(string(r)))
	}
	if got, want := m.prompt, "df -h"; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}

	m = send(m, tea.KeyMsg{Type: tea.KeyBackspace})
	if got, want := m.prompt, "df -"; got != want {
		t.Errorf("after backspace prompt = %q, want %q", got, want)
	}

	m = send(m, key("esc"))
	if m.view != viewTable || m.prompt != "" {
		t.Errorf("esc left view=%v prompt=%q, want table and empty", m.view, m.prompt)
	}
}

func TestRunningCommandSendsScriptToEveryTarget(t *testing.T) {
	m, _ := testModel(t)
	fake := &fakeExecutor{result: collect.ExecResult{Output: "ok\n"}}
	m.executor = fake

	m = send(m, key("a"), key("x"))
	for _, r := range "uptime" {
		m = send(m, key(string(r)))
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter produced no command")
	}
	msg := cmd()
	done, ok := msg.(execDoneMsg)
	if !ok {
		t.Fatalf("got %T, want execDoneMsg", msg)
	}

	if got, want := len(done.results), 3; got != want {
		t.Errorf("results = %d, want %d", got, want)
	}
	if got, want := fake.lastScript(), "uptime"; got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
	// Results keep the on-screen host order regardless of which answered first.
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if done.results[i].Host != want {
			t.Errorf("result %d is %q, want %q", i, done.results[i].Host, want)
		}
	}
}

func TestNonZeroExitIsReportedNotTreatedAsError(t *testing.T) {
	m, _ := testModel(t)
	m.executor = &fakeExecutor{result: collect.ExecResult{Output: "nope\n", ExitCode: 2}}
	m = send(m, key("x"))
	for _, r := range "false" {
		m = send(m, key(string(r)))
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	done := cmd().(execDoneMsg)
	r := done.results[0]
	if r.Err != nil {
		t.Errorf("non-zero exit surfaced as an error: %v", r.Err)
	}
	if r.ExitCode != 2 || r.OK() {
		t.Errorf("exit code = %d, OK = %v; want 2 and false", r.ExitCode, r.OK())
	}

	m = send(m, done)
	if m.view != viewResults {
		t.Error("results view did not open")
	}
	if out := m.View(); !strings.Contains(out, "exit 2") || !strings.Contains(out, "nope") {
		t.Errorf("results view missing exit code or output:\n%s", out)
	}
}

func TestEmptyCommandDoesNothing(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, key("x"))
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("empty prompt issued a command")
	}
	if next.(Model).view != viewTable {
		t.Error("empty prompt did not return to the table")
	}
}

func TestScriptFileIsReadAndPiped(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/check.sh"
	body := "#!/bin/sh\nuptime\ndf -h /\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m, _ := testModel(t)
	fake := &fakeExecutor{}
	m.executor = fake
	m = send(m, key("x"))
	m.prompt = "@" + path

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cmd()

	// The file contents go over the wire, not the path — nothing is written to
	// the monitored machines.
	if fake.lastScript() != body {
		t.Errorf("script = %q, want the file contents %q", fake.lastScript(), body)
	}
}

func TestMissingScriptFileReportsInsteadOfSilentlyDropping(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, key("x"))
	m.prompt = "@/no/such/script.sh"

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil {
		t.Error("a missing script still tried to run")
	}
	if m.view != viewResults {
		t.Fatalf("view = %v, want the results view showing the error", m.view)
	}
	if out := m.View(); !strings.Contains(out, "no such") && !strings.Contains(out, "no/such") {
		t.Errorf("error not surfaced:\n%s", out)
	}
}

func TestResultsScrollAndClose(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, tea.WindowSizeMsg{Width: 80, Height: 10})
	m.view = viewResults
	m.results = []execResult{{Host: "alpha", Output: strings.Repeat("line\n", 50)}}

	m = send(m, key("down"), key("down"))
	if m.resultScroll != 2 {
		t.Errorf("resultScroll = %d, want 2", m.resultScroll)
	}
	m = send(m, key("up"), key("up"), key("up"))
	if m.resultScroll != 0 {
		t.Errorf("resultScroll = %d, want 0 (must not go negative)", m.resultScroll)
	}

	m = send(m, key("esc"))
	if m.view != viewTable {
		t.Error("esc did not close the results view")
	}
	// Results survive closing, so reopening does not lose them.
	if len(m.results) == 0 {
		t.Error("results were discarded on close")
	}
}

func TestResolveScript(t *testing.T) {
	if got, _ := resolveScript("  df -h  "); got != "df -h" {
		t.Errorf("resolveScript trimmed to %q, want %q", got, "df -h")
	}
	if _, err := resolveScript("@"); err == nil {
		t.Error("bare @ accepted, want an error about the missing path")
	}
}

// TestRootCommandAsksForPasswordBeforeSendingAnything is the property that
// matters: X must not reach a host until a password has been entered.
func TestRootCommandAsksForPasswordBeforeSendingAnything(t *testing.T) {
	m, _ := testModel(t)
	fake := &fakeExecutor{}
	m.executor = fake

	m = send(m, key("X"))
	if m.view != viewPrompt || !m.asRoot {
		t.Fatalf("X gave view=%v asRoot=%v, want the prompt in root mode", m.view, m.asRoot)
	}
	if out := m.View(); !strings.Contains(out, "RUN AS ROOT") {
		t.Errorf("prompt does not announce root:\n%s", out)
	}

	for _, r := range "apt update" {
		m = send(m, key(string(r)))
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)

	if cmd != nil {
		t.Error("root command ran before a password was entered")
	}
	if m.view != viewPassword {
		t.Fatalf("view = %v, want the password prompt", m.view)
	}
	if len(fake.addrs) != 0 {
		t.Errorf("executor was called before the password: %v", fake.addrs)
	}
}

func TestPasswordIsNeverRendered(t *testing.T) {
	m, _ := testModel(t)
	m = send(m, key("X"))
	for _, r := range "whoami" {
		m = send(m, key(string(r)))
	}
	m = send(m, tea.KeyMsg{Type: tea.KeyEnter})

	for _, r := range "hunter2" {
		m = send(m, key(string(r)))
	}
	out := m.View()
	if strings.Contains(out, "hunter2") {
		t.Errorf("password appears on screen:\n%s", out)
	}
	// Only the length shows, so you can see it registered your typing.
	if !strings.Contains(out, strings.Repeat("•", len("hunter2"))) {
		t.Errorf("password prompt shows no masked characters:\n%s", out)
	}
}

func TestRootCommandSendsPasswordAndRootFlag(t *testing.T) {
	m, _ := testModel(t)
	fake := &fakeExecutor{result: collect.ExecResult{Output: "root\n"}}
	m.executor = fake

	m = send(m, key("a"), key("X"))
	for _, r := range "whoami" {
		m = send(m, key(string(r)))
	}
	m = send(m, tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "s3cret" {
		m = send(m, key(string(r)))
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("no command issued after entering the password")
	}
	cmd()

	script, asRoot, password := fake.lastRequest()
	if script != "whoami" || !asRoot || password != "s3cret" {
		t.Errorf("request = %q root=%v pw=%q, want whoami/true/s3cret", script, asRoot, password)
	}

	// The model must not keep the secret once the run owns it.
	if m.password != "" {
		t.Errorf("model still holds the password after launching the run")
	}
}

func TestCancellingPasswordClearsIt(t *testing.T) {
	m, _ := testModel(t)
	fake := &fakeExecutor{}
	m.executor = fake

	m = send(m, key("X"))
	for _, r := range "reboot" {
		m = send(m, key(string(r)))
	}
	m = send(m, tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "secret" {
		m = send(m, key(string(r)))
	}
	m = send(m, key("esc"))

	if m.password != "" {
		t.Error("password survived cancelling the prompt")
	}
	if m.asRoot {
		t.Error("root mode survived cancelling")
	}
	if m.view != viewTable {
		t.Errorf("view = %v, want the table", m.view)
	}
	if len(fake.addrs) != 0 {
		t.Errorf("something ran despite cancelling: %v", fake.addrs)
	}
}

func TestPlainCommandNeverSetsRootOrPassword(t *testing.T) {
	m, _ := testModel(t)
	fake := &fakeExecutor{}
	m.executor = fake

	m = send(m, key("x"))
	for _, r := range "uptime" {
		m = send(m, key(string(r)))
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cmd()

	_, asRoot, password := fake.lastRequest()
	if asRoot || password != "" {
		t.Errorf("plain command sent root=%v pw=%q, want false and empty", asRoot, password)
	}
}

func TestEmptyPasswordDoesNotRun(t *testing.T) {
	m, _ := testModel(t)
	fake := &fakeExecutor{}
	m.executor = fake

	m = send(m, key("X"))
	for _, r := range "id" {
		m = send(m, key(string(r)))
	}
	m = send(m, tea.KeyMsg{Type: tea.KeyEnter})

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // enter with nothing typed
	if cmd != nil {
		t.Error("ran with an empty password")
	}
	if next.(Model).view != viewPassword {
		t.Error("left the password prompt without a password")
	}
}
