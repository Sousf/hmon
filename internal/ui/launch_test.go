package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sousf/hmon/internal/config"
	"github.com/Sousf/hmon/internal/model"
)

const templateConfig = `
hosts: [alpha, beta]
templates:
  - name: nixos
    image: images:nixos/25.05/cloud
    type: vm
    cpu: 4
    memory: 4GiB
  - name: alpine
    image: images:alpine/3.20
`

// launchModel builds a model whose config actually has templates, which the
// shared testModel deliberately does not.
func launchModel(t *testing.T, extra string) (Model, *model.Fleet) {
	t.Helper()
	cfg, err := config.Parse([]byte(templateConfig + extra))
	if err != nil {
		t.Fatal(err)
	}
	fleet := model.NewFleet(cfg.HostRefs())
	m := New(cfg, fleet, &fakePoller{}, &fakeExecutor{}, &fakeExecutor{})
	m.width, m.height = 100, 24
	return m, fleet
}

func launcher(m Model) *fakeExecutor { return m.launcher.(*fakeExecutor) }

func backspace() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyBackspace} }

func typeName(m Model, name string) Model {
	for _, r := range name {
		m = send(m, key(string(r)))
	}
	return m
}

func TestLaunchFlowReachesConfirmAndRuns(t *testing.T) {
	m, _ := launchModel(t, "")
	m.selected = "beta"

	m = send(m, key("n"))
	if m.view != viewLaunch || m.launch.step != stepTemplate {
		t.Fatalf("view=%v step=%v", m.view, m.launch.step)
	}
	if m.launch.host != "beta" {
		t.Errorf("host = %q, want beta", m.launch.host)
	}

	m = send(m, key("enter")) // take the first template, nixos
	if m.launch.step != stepName {
		t.Fatalf("step = %v, want name", m.launch.step)
	}

	m = typeName(m, "build-01")
	m = send(m, key("enter"))
	if m.launch.step != stepConfirm {
		t.Fatalf("step = %v, want confirm", m.launch.step)
	}

	// Update rather than send, because the command it returns is the launch and
	// send discards it.
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("confirming produced no command")
	}
	drain(cmd)

	script := launcher(m).lastScript()
	if !strings.Contains(script, "lxc launch 'images:nixos/25.05/cloud' build-01 --vm") {
		t.Errorf("wrong launch command:\n%s", script)
	}
	if !strings.Contains(script, "limits.cpu=4") || !strings.Contains(script, "limits.memory='4GiB'") {
		t.Errorf("template limits missing:\n%s", script)
	}
	// The output lands in the results view, which is what the fan-out already
	// fills — a launch is one host's output with an exit code.
	if m.view != viewResults {
		t.Errorf("view = %v, want results", m.view)
	}
}

// The launcher has its own deadline; a launch must not be billed against the
// one-minute ad-hoc command budget.
func TestLaunchUsesTheLauncherNotTheCommandExecutor(t *testing.T) {
	m, _ := launchModel(t, "")
	m.selected = "alpha"
	m = send(m, key("n"), key("enter"))
	m = typeName(m, "web")
	m = send(m, key("enter"))

	next, cmd := m.startLaunch()
	m = next.(Model)
	drain(cmd)

	if launcher(m).lastScript() == "" {
		t.Error("the launcher was never called")
	}
	if got := m.executor.(*fakeExecutor).lastScript(); got != "" {
		t.Errorf("the command executor ran a launch: %q", got)
	}
}

func TestLaunchRefusedWithoutTemplatesOrOnGuests(t *testing.T) {
	// No templates at all: n has nothing to offer and must not open an empty
	// picker.
	plain, _ := testModel(t, "alpha")
	if got := send(plain, key("n")); got.view == viewLaunch {
		t.Error("n opened the launch flow with no templates configured")
	}

	// A guest cannot host anything: this runs over ssh to an address it has
	// none of.
	m, f := launchModel(t, "")
	m = seedGuests(t, m, f, "alpha", "nixos")
	m.selected = "alpha/nixos"
	if got := send(m, key("n")); got.view == viewLaunch {
		t.Error("n opened the launch flow on a guest row")
	}
}

func TestLaunchNameIsValidatedBeforeAnythingIsSent(t *testing.T) {
	m, _ := launchModel(t, "")
	m = send(m, key("n"), key("enter"))

	m = typeName(m, "bad name")
	m = send(m, key("enter"))
	if m.launch.step != stepName {
		t.Fatal("an invalid name was accepted")
	}
	if m.launch.err == "" {
		t.Error("no reason was shown for the rejection")
	}
	if launcher(m).lastScript() != "" {
		t.Error("something was sent despite the invalid name")
	}

	// Correcting it clears the message and moves on.
	for range "bad name" {
		m = send(m, backspace())
	}
	m = typeName(m, "good-name")
	m = send(m, key("enter"))
	if m.launch.step != stepConfirm || m.launch.err != "" {
		t.Errorf("step=%v err=%q", m.launch.step, m.launch.err)
	}
}

// Only y proceeds, matching the reboot dialog: there is no confirming a launch
// by mashing keys.
func TestOnlyYConfirmsALaunch(t *testing.T) {
	for _, k := range []string{"n", "enter", " ", "Y", "q"} {
		m, _ := launchModel(t, "")
		m = send(m, key("n"), key("enter"))
		m = typeName(m, "web")
		m = send(m, key("enter"))

		got := send(m, key(k))
		if got.view != viewTable {
			t.Errorf("%q did not cancel: view = %v", k, got.view)
		}
		if launcher(got).lastScript() != "" {
			t.Errorf("%q launched something", k)
		}
	}
}

func TestLaunchCanBeAbandonedAtEveryStep(t *testing.T) {
	m, _ := launchModel(t, "")

	if got := send(m, key("n"), key("esc")); got.view != viewTable {
		t.Error("esc did not leave the template picker")
	}

	// From the name field, esc goes back to the picker rather than out: changing
	// your mind about the template is likelier than abandoning the launch.
	back := send(m, key("n"), key("enter"), key("esc"))
	if back.view != viewLaunch || back.launch.step != stepTemplate {
		t.Errorf("esc from the name field went to view=%v step=%v", back.view, back.launch.step)
	}
}

// A name typed and abandoned must not turn up in the next launch.
func TestLaunchStateResetsBetweenLaunches(t *testing.T) {
	m, _ := launchModel(t, "")
	m = send(m, key("n"), key("enter"))
	m = typeName(m, "abandoned")
	m = send(m, key("esc"), key("esc"))

	m = send(m, key("n"))
	if m.launch.name != "" || m.launch.template != 0 || m.launch.err != "" {
		t.Errorf("stale state: %+v", m.launch)
	}
}

// The provision file is read locally, before anything is created, so a missing
// script is reported instead of leaving a VM waiting to be set up.
func TestMissingProvisionScriptIsReportedBeforeLaunching(t *testing.T) {
	m, _ := launchModel(t, "\n    provision: ./nowhere.sh\n")
	m.cfg.Path = filepath.Join(t.TempDir(), "config.yaml")
	m.selected = "alpha"

	m = send(m, key("n"), key("down"), key("enter")) // alpine, which has the script
	m = typeName(m, "web")
	m = send(m, key("enter"))

	next, _ := m.startLaunch()
	m = next.(Model)

	if m.view != viewResults || len(m.results) != 1 {
		t.Fatalf("view=%v results=%d", m.view, len(m.results))
	}
	if m.results[0].Err == nil {
		t.Fatal("a missing provision script was not reported")
	}
	if launcher(m).lastScript() != "" {
		t.Error("a launch was sent despite the unreadable script")
	}
}

func TestProvisionScriptIsEmbeddedFromDisk(t *testing.T) {
	dir := t.TempDir()
	body := "#!/bin/sh\necho 'provisioned'\n"
	if err := os.WriteFile(filepath.Join(dir, "setup.sh"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	m, _ := launchModel(t, "\n    provision: ./setup.sh\n")
	m.cfg.Path = filepath.Join(dir, "config.yaml")
	m.selected = "alpha"

	m = send(m, key("n"), key("down"), key("enter"))
	m = typeName(m, "web")
	m = send(m, key("enter"))

	next, cmd := m.startLaunch()
	m = next.(Model)
	drain(cmd)

	script := launcher(m).lastScript()
	if !strings.Contains(script, "provisioned") {
		t.Errorf("the script's contents never reached the launch:\n%s", script)
	}
	if !strings.Contains(script, "lxc exec web -- sh -s") {
		t.Errorf("no provisioning step:\n%s", script)
	}
}

func TestTemplatePickerMovesAndRenders(t *testing.T) {
	m, _ := launchModel(t, "")
	m = send(m, key("n"))

	m = send(m, key("down"))
	if m.launch.template != 1 {
		t.Errorf("template = %d, want 1", m.launch.template)
	}
	// Clamped at both ends rather than wrapping, like the table cursor.
	m = send(m, key("down"), key("down"))
	if m.launch.template != 1 {
		t.Errorf("template = %d, want it clamped at 1", m.launch.template)
	}
	m = send(m, key("up"), key("up"))
	if m.launch.template != 0 {
		t.Errorf("template = %d, want it clamped at 0", m.launch.template)
	}

	out := m.View()
	for _, want := range []string{"nixos", "images:nixos/25.05/cloud", "vm, 4 cpu, 4GiB", "alpine"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker does not show %q:\n%s", want, out)
		}
	}
}

// The confirmation shows the command itself, not a paraphrase — this is the
// moment to notice a wrong image, and a summary is the one thing you could not
// check.
func TestConfirmShowsTheExactCommand(t *testing.T) {
	m, _ := launchModel(t, "")
	m.selected = "beta"
	m = send(m, key("n"), key("enter"))
	m = typeName(m, "build-01")
	m = send(m, key("enter"))

	out := m.View()
	if !strings.Contains(out, "lxc launch 'images:nixos/25.05/cloud' build-01 --vm") {
		t.Errorf("confirm screen does not show the command:\n%s", out)
	}
	if !strings.Contains(out, "beta") {
		t.Errorf("confirm screen does not name the host:\n%s", out)
	}
}

// n is only advertised when it would do something.
func TestNewBindingShownOnlyWithTemplates(t *testing.T) {
	withTemplates, _ := launchModel(t, "")
	withTemplates.height = 8
	if !strings.Contains(withTemplates.renderTable(), "n new") {
		t.Error("n is not offered despite templates being configured")
	}

	plain, _ := testModel(t, "alpha")
	plain.width, plain.height = 100, 8
	if strings.Contains(plain.renderTable(), "n new") {
		t.Error("n is advertised with no templates to launch")
	}
}

// A real `lxc launch` writes its download progress as a hundred carriage-return
// redraws on a single line. Replayed as text they all survive, burying the
// output; this view never streams, so only the final frame was ever meaningful.
func TestProgressRedrawsCollapseToTheirFinalFrame(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Retrieving image: 1%\rRetrieving image: 50%\rRetrieving image: 100%", "Retrieving image: 100%"},
		{"plain line", "plain line"},
		{"", ""},
		{"trailing\r", ""},
		{"\rleading", "leading"},
	}
	for _, c := range cases {
		if got := collapseProgress(c.in); got != c.want {
			t.Errorf("collapseProgress(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	m, _ := launchModel(t, "")
	m.width, m.height = 100, 40
	m.results = []execResult{{
		Host:   "hermes3",
		Output: "==> launching web\nRetrieving image: 4%\rRetrieving image: 71%\rRetrieving image: 100%\n==> web is ready\n",
	}}
	out := m.renderResults()
	if strings.Contains(out, "4%") || strings.Contains(out, "71%") {
		t.Errorf("intermediate progress frames survived:\n%s", out)
	}
	if !strings.Contains(out, "100%") {
		t.Errorf("the final frame was lost:\n%s", out)
	}
	if !strings.Contains(out, "web is ready") {
		t.Errorf("output after the progress bar was lost:\n%s", out)
	}
}
