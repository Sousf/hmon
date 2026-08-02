package ui

import (
	"context"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sousf/hmon/internal/collect"
	"github.com/Sousf/hmon/internal/config"
)

// launchStep is where the launch flow has got to. One screen answers one
// question: which template, what to call it, and are you sure.
type launchStep int

const (
	stepTemplate launchStep = iota
	stepName
	stepConfirm
)

// launchState is everything the flow is holding. It is reset on entry rather
// than carried between launches, so a name typed and abandoned cannot leak into
// the next one.
type launchState struct {
	step     launchStep
	host     string // the machine to create on
	template int    // index into cfg.Templates
	name     string
	// err is a validation message shown under the name field. Kept as text
	// rather than an error because it is only ever displayed.
	err string
}

// handleLaunchKey drives the launch flow.
func (m Model) handleLaunchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.launch.step {
	case stepTemplate:
		return m.handleTemplateKey(msg)
	case stepName:
		return m.handleLaunchNameKey(msg)
	default:
		return m.handleLaunchConfirmKey(msg)
	}
}

func (m Model) handleTemplateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.view = viewTable
	case "up", "k":
		if m.launch.template > 0 {
			m.launch.template--
		}
	case "down", "j":
		if m.launch.template < len(m.cfg.Templates)-1 {
			m.launch.template++
		}
	case "enter":
		m.launch.step = stepName
	}
	return m, nil
}

func (m Model) handleLaunchNameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		// Back to the picker rather than out of the flow: changing your mind
		// about the template is likelier than abandoning the launch.
		m.launch.step = stepTemplate
		m.launch.err = ""
		return m, nil
	case tea.KeyCtrlC:
		m.view = viewTable
		return m, nil
	case tea.KeyBackspace:
		if n := len([]rune(m.launch.name)); n > 0 {
			m.launch.name = string([]rune(m.launch.name)[:n-1])
		}
		m.launch.err = ""
		return m, nil
	case tea.KeyEnter:
		// Validated here rather than on the host, so a typo costs a keystroke
		// instead of a round trip.
		if err := collect.ValidInstanceName(m.launch.name); err != nil {
			m.launch.err = err.Error()
			return m, nil
		}
		m.launch.err = ""
		m.launch.step = stepConfirm
		return m, nil
	case tea.KeyRunes, tea.KeySpace:
		m.launch.name += string(msg.Runes)
		m.launch.err = ""
		return m, nil
	}
	return m, nil
}

// handleLaunchConfirmKey resolves the final screen. Only "y" proceeds, matching
// how the reboot dialog works: creating a machine is not destructive, but it is
// still the tool acting on the world rather than reading it.
func (m Model) handleLaunchConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() != "y" {
		m.view = viewTable
		return m, nil
	}
	return m.startLaunch()
}

// startLaunch builds the script and hands it to the launcher.
//
// The provision file is read here, locally, so a missing or unreadable script
// is reported before anything is created — rather than after a VM exists and is
// waiting to be provisioned.
func (m Model) startLaunch() (tea.Model, tea.Cmd) {
	t := m.template()
	host, ok := m.fleet.Get(m.launch.host)
	if !ok {
		m.view = viewTable
		return m, nil
	}

	provision, err := readProvision(m.cfg, t)
	if err != nil {
		m.view = viewResults
		m.results = []execResult{{Host: m.launch.host, Err: err}}
		m.resultScroll = 0
		return m, nil
	}

	script, err := collect.LaunchScript(collect.LaunchRequest{
		Name:      m.launch.name,
		Image:     t.Image,
		VM:        t.IsVM(),
		CPU:       t.CPU,
		Memory:    t.Memory,
		Provision: provision,
	})
	if err != nil {
		m.view = viewResults
		m.results = []execResult{{Host: m.launch.host, Err: err}}
		m.resultScroll = 0
		return m, nil
	}

	m.view = viewResults
	m.running = true
	m.results = nil
	m.resultScroll = 0
	return m, m.runLaunch(script, host.Addr, host.Name)
}

// runLaunch executes the launch script on one host under the launch deadline.
//
// It reuses the results view the fan-out already fills: a launch is one host's
// output with an exit code, which is exactly what that view exists to show.
func (m Model) runLaunch(script, addr, name string) tea.Cmd {
	launcher := m.launcher
	timeout := m.cfg.LaunchTimeout

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		res, err := launcher.Exec(ctx, addr, collect.ExecRequest{Script: script})
		return execDoneMsg{results: []execResult{{
			Host:     name,
			Output:   res.Output,
			ExitCode: res.ExitCode,
			Err:      err,
		}}}
	}
}

// readProvision loads a template's provision script from disk.
func readProvision(cfg *config.Config, t config.Template) (string, error) {
	path := cfg.ProvisionFor(t)
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(expandHome(path))
	if err != nil {
		return "", fmt.Errorf("provision script for template %q: %w", t.Name, err)
	}
	return string(body), nil
}

// template returns the currently picked template, guarding the index so a
// config reloaded out from under the flow cannot panic it.
func (m Model) template() config.Template {
	if m.launch.template < 0 || m.launch.template >= len(m.cfg.Templates) {
		return config.Template{}
	}
	return m.cfg.Templates[m.launch.template]
}

// renderLaunch draws whichever step is current, over the table.
func (m Model) renderLaunch() string {
	var b strings.Builder
	b.WriteString(m.renderTableOnly())
	b.WriteString("\n")
	b.WriteString(styleFaint.Render(separator(m.width)))
	b.WriteString("\n\n")

	b.WriteString("  " + styleHeader.Render("NEW INSTANCE ON ") +
		styleSelected.Render(m.launch.host) + "\n\n")

	switch m.launch.step {
	case stepTemplate:
		b.WriteString(m.renderTemplatePicker())
	case stepName:
		b.WriteString(m.renderLaunchName())
	default:
		b.WriteString(m.renderLaunchConfirm())
	}
	return b.String()
}

func (m Model) renderTemplatePicker() string {
	var b strings.Builder
	for i, t := range m.cfg.Templates {
		cursor := "    "
		name := styleText.Render(padRight(t.Name, 14))
		if i == m.launch.template {
			cursor = "  " + styleSelected.Render("▸ ")
			name = styleSelected.Render(padRight(t.Name, 14))
		}
		// The image and shape are shown beside every entry rather than only for
		// the highlighted one: choosing between templates means comparing them,
		// and a picker that reveals one at a time makes you step through all of
		// them to do it.
		b.WriteString(cursor + name + " " + styleDim.Render(t.Image) +
			" " + styleFaint.Render(templateShape(t)) + "\n")
	}
	b.WriteString("\n  " + renderHelp(
		helpItem{"↑↓", "choose"},
		helpItem{"enter", "next"},
		helpItem{"esc", "cancel"},
	))
	return b.String()
}

// templateShape summarises what a template asks for, omitting anything unset
// rather than printing a zero that looks like a limit.
func templateShape(t config.Template) string {
	parts := []string{t.Type}
	if t.CPU > 0 {
		parts = append(parts, fmt.Sprintf("%d cpu", t.CPU))
	}
	if t.Memory != "" {
		parts = append(parts, t.Memory)
	}
	if t.Provision != "" {
		parts = append(parts, "+provision")
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func (m Model) renderLaunchName() string {
	t := m.template()

	var b strings.Builder
	b.WriteString("  " + styleDim.Render(t.Name+" · "+t.Image) + "\n\n")
	b.WriteString("  " + styleHeader.Render("name") + " " +
		styleText.Render(m.launch.name) + styleSelected.Render("█") + "\n")
	if m.launch.err != "" {
		b.WriteString("  " + styleCrit.Render(m.launch.err) + "\n")
	}
	b.WriteString("\n  " + renderHelp(
		helpItem{"enter", "next"},
		helpItem{"esc", "back"},
	))
	return b.String()
}

func (m Model) renderLaunchConfirm() string {
	t := m.template()

	var b strings.Builder
	// The exact command is shown, not a summary of it. This is the moment to
	// notice that the image is wrong or a limit is missing, and a paraphrase
	// would be the one thing you could not check.
	b.WriteString("  " + styleDim.Render("on "+m.launch.host+":") + "\n")
	b.WriteString("  " + styleText.Render(collect.LaunchCommand(collect.LaunchRequest{
		Name:   m.launch.name,
		Image:  t.Image,
		VM:     t.IsVM(),
		CPU:    t.CPU,
		Memory: t.Memory,
	})) + "\n\n")

	if t.Provision != "" {
		b.WriteString("  " + styleDim.Render("then provision with ") +
			styleText.Render(m.cfg.ProvisionFor(t)) + "\n\n")
	}

	b.WriteString("  " + styleWarn.Render("Create it?") + " " +
		styleDim.Render("this may take several minutes on a first launch") + "\n\n")
	b.WriteString("  " + renderHelp(
		helpItem{"y", "create"},
		helpItem{"any other key", "cancel"},
	))
	return b.String()
}
