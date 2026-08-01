package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sousf/hmon/internal/collect"
	"github.com/Sousf/hmon/internal/model"
)

// scriptPrefix marks the command text as a path to a local script rather than
// a command to run. The file is read here and piped to the remote shell, so
// nothing is written to the monitored machines — the same mechanism the
// collector uses.
const scriptPrefix = "@"

// Executor runs a script on a host. Narrowed to what the UI calls so tests can
// substitute one without touching SSH.
type Executor interface {
	Exec(ctx context.Context, addr string, req collect.ExecRequest) (collect.ExecResult, error)
}

// execResult pairs a host with what running the script on it produced.
type execResult struct {
	Host     string
	Output   string
	ExitCode int
	Err      error
}

// OK reports whether the command succeeded outright.
func (r execResult) OK() bool { return r.Err == nil && r.ExitCode == 0 }

type execDoneMsg struct{ results []execResult }

// targets returns the hosts a command would run on: everything marked, or the
// host under the cursor when nothing is marked.
//
// Defaulting to the selection rather than to the whole fleet is deliberate.
// The dangerous mistake here is running somewhere you did not mean to, and an
// empty mark set is far more likely to mean "just this one" than "all of
// them".
func (m Model) targets() []*model.Host {
	var out []*model.Host
	for _, h := range m.sortedHosts() {
		if m.marked[h.Name] {
			out = append(out, h)
		}
	}
	if len(out) > 0 {
		return out
	}
	if h, ok := m.fleet.Get(m.selected); ok {
		return []*model.Host{h}
	}
	return nil
}

// resolveScript turns the typed text into the script to send. A leading @ is
// a local file path; anything else is used verbatim, since a one-line command
// is just a very short script.
func resolveScript(input string) (string, error) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, scriptPrefix) {
		return input, nil
	}
	path := strings.TrimSpace(strings.TrimPrefix(input, scriptPrefix))
	if path == "" {
		return "", fmt.Errorf("no script path after %q", scriptPrefix)
	}
	body, err := os.ReadFile(expandHome(path))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// expandHome handles a leading ~ , which the shell would normally expand
// before a program ever sees it — but this path was typed into our own prompt,
// so no shell was involved.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return home + path[1:]
}

// runOnTargets executes the script on every target concurrently and returns a
// single message once they have all finished.
//
// Results are collected into a slice indexed by position rather than appended
// from each goroutine, so the output order matches the host order on screen no
// matter which machine answers first.
func (m Model) runOnTargets(script string, hosts []*model.Host) tea.Cmd {
	executor := m.executor
	timeout := m.cfg.CommandTimeout

	// Captured by value into the closure and cleared from the model by the
	// caller, so the password lives no longer than the run that needs it.
	req := collect.ExecRequest{
		Script:   script,
		AsRoot:   m.asRoot,
		Password: m.password,
	}

	names := make([]string, len(hosts))
	addrs := make([]string, len(hosts))
	for i, h := range hosts {
		names[i], addrs[i] = h.Name, h.Addr
	}

	return func() tea.Msg {
		results := make([]execResult, len(names))
		var wg sync.WaitGroup

		for i := range names {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()

				res, err := executor.Exec(ctx, addrs[i], req)
				results[i] = execResult{
					Host:     names[i],
					Output:   res.Output,
					ExitCode: res.ExitCode,
					Err:      err,
				}
			}(i)
		}
		wg.Wait()
		return execDoneMsg{results: results}
	}
}

// renderPrompt draws the command entry line.
//
// The target hosts are shown while you type, not on a later confirmation
// screen. Scope is the thing that goes wrong with fan-out — running on three
// machines when you meant one — so it stays in front of you at the moment you
// are deciding what to type.
func (m Model) renderPrompt() string {
	hosts := m.targets()
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Name)
	}

	var b strings.Builder
	b.WriteString(m.renderTableOnly())
	b.WriteString("\n")
	b.WriteString(styleDim.Render(separator(m.width)))
	b.WriteString("\n\n")

	scope := styleSelected.Render(fmt.Sprintf("%d host%s", len(names), plural(len(names)))) +
		styleDim.Render(": "+strings.Join(names, ", "))

	label := styleHeader.Render("RUN ON ")
	if m.asRoot {
		// Root runs are called out in the warning colour: the difference
		// between this and an ordinary command is the whole machine.
		label = styleWarn.Render("RUN AS ROOT ON ")
	}
	b.WriteString("  " + label + scope + "\n\n")

	b.WriteString("  " + styleAccentPrompt(">") + " " + styleText.Render(m.prompt) +
		styleSelected.Render("█") + "\n\n")

	b.WriteString("  " + renderHelp(
		helpItem{"enter", "run"},
		helpItem{"esc", "cancel"},
		helpItem{"@path", "runs a local script file"},
	))
	return b.String()
}

// renderPassword draws the sudo password prompt.
//
// The typed characters are never rendered, only their count, so the password
// cannot be read off the screen or captured in a screenshot of the terminal.
func (m Model) renderPassword() string {
	hosts := m.targets()
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Name)
	}

	var b strings.Builder
	b.WriteString(m.renderTableOnly())
	b.WriteString("\n")
	b.WriteString(styleDim.Render(separator(m.width)))
	b.WriteString("\n\n")

	b.WriteString("  " + styleWarn.Render("RUN AS ROOT ON ") +
		styleSelected.Render(fmt.Sprintf("%d host%s", len(names), plural(len(names)))) +
		styleDim.Render(": "+strings.Join(names, ", ")) + "\n")
	b.WriteString("  " + styleDim.Render("$ "+truncate(m.prompt, maxInt(20, m.width-6))) + "\n\n")

	b.WriteString("  " + styleHeader.Render("sudo password") + " " +
		styleText.Render(strings.Repeat("•", len([]rune(m.password)))) +
		styleSelected.Render("█") + "\n\n")

	b.WriteString("  " + renderHelp(
		helpItem{"enter", "run"},
		helpItem{"esc", "cancel"},
	))
	b.WriteString(styleDim.Render("   the password is used once and discarded"))
	return b.String()
}

// renderResults draws the per-host output of the last command.
func (m Model) renderResults() string {
	var lines []string

	failed := 0
	for _, r := range m.results {
		if !r.OK() {
			failed++
		}
	}

	header := styleTitle.Render("results")
	if failed == 0 {
		header += "  " + styleOK.Render(fmt.Sprintf("%d ok", len(m.results)))
	} else {
		header += "  " + styleCrit.Render(fmt.Sprintf("%d failed", failed)) +
			styleDim.Render(fmt.Sprintf(" of %d", len(m.results)))
	}
	lines = append(lines, header, "")

	for _, r := range m.results {
		lines = append(lines, m.resultBlock(r)...)
	}

	// Scroll rather than truncate: output from several hosts routinely runs
	// past a screen, and silently cutting it off would hide the tail where
	// errors usually are.
	//
	// Height is zero until the first WindowSizeMsg. Clamping to a minimum then
	// would show a header and nothing else, so an unknown height means show
	// everything and let the terminal do the clipping.
	body := lines
	visible := len(lines)
	if m.height > 0 {
		visible = m.height - 3
		if visible < 1 {
			visible = 1
		}
	}
	if len(body) > visible {
		start := m.resultScroll
		if start > len(body)-visible {
			start = len(body) - visible
		}
		if start < 0 {
			start = 0
		}
		body = body[start : start+visible]
	}

	out := strings.Join(body, "\n")
	more := ""
	if len(lines) > visible {
		more = fmt.Sprintf(" · %d/%d lines", min(m.resultScroll+visible, len(lines)), len(lines))
	}
	help := renderHelp(
		helpItem{"↑↓", "scroll"},
		helpItem{"esc", "close"},
	)
	return out + "\n\n" + help + styleDim.Render(more)
}

func (m Model) resultBlock(r execResult) []string {
	var status string
	switch {
	case r.Err != nil:
		status = styleCrit.Render("error: " + truncate(r.Err.Error(), maxInt(20, m.width-24)))
	case r.ExitCode != 0:
		status = styleCrit.Render(fmt.Sprintf("exit %d", r.ExitCode))
	default:
		status = styleOK.Render("ok")
	}

	lines := []string{"  " + styleSelected.Render(r.Host) + "  " + status}

	body := strings.TrimRight(r.Output, "\n")
	if body == "" {
		if r.Err == nil {
			lines = append(lines, "    "+styleDim.Render("(no output)"))
		}
	} else {
		for _, ln := range strings.Split(body, "\n") {
			lines = append(lines, "    "+styleText.Render(truncate(ln, maxInt(20, m.width-6))))
		}
	}
	return append(lines, "")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func styleAccentPrompt(s string) string { return styleSelected.Render(s) }

// handlePromptKey drives command entry. Every key belongs to the prompt while
// it is open, so a command containing "q" or "r" cannot trigger the bindings
// underneath.
func (m Model) handlePromptKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.view = viewTable
		m.prompt = ""
		return m, nil

	case tea.KeyEnter:
		script, err := resolveScript(m.prompt)
		if err != nil {
			// A bad script path is reported the same way a failed command is,
			// rather than silently dropping what was typed.
			m.view = viewResults
			m.results = []execResult{{Host: "—", Err: err}}
			m.resultScroll = 0
			return m, nil
		}
		if strings.TrimSpace(script) == "" {
			m.view = viewTable
			return m, nil
		}

		if m.asRoot {
			// Nothing is sent until the password is entered, so the command
			// text is kept and the run deferred to the password prompt.
			m.view = viewPassword
			m.password = ""
			return m, nil
		}

		hosts := m.targets()
		m.view = viewResults
		m.running = true
		m.results = nil
		m.resultScroll = 0
		return m, m.runOnTargets(script, hosts)

	case tea.KeyBackspace:
		if r := []rune(m.prompt); len(r) > 0 {
			m.prompt = string(r[:len(r)-1])
		}
		return m, nil

	case tea.KeyCtrlU:
		m.prompt = ""
		return m, nil

	case tea.KeyCtrlC:
		m.quitting = true
		return m, tea.Quit

	case tea.KeySpace:
		m.prompt += " "
		return m, nil

	case tea.KeyRunes:
		m.prompt += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

// handleResultsKey scrolls the output view.
func (m Model) handleResultsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		// Results are kept, so closing and reopening with x does not lose them
		// until the next command replaces them.
		m.view = viewTable
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.resultScroll > 0 {
			m.resultScroll--
		}
		return m, nil
	case "down", "j":
		m.resultScroll++
		return m, nil
	case "pgup":
		m.resultScroll -= 10
		if m.resultScroll < 0 {
			m.resultScroll = 0
		}
		return m, nil
	case "pgdown":
		m.resultScroll += 10
		return m, nil
	case "g":
		m.resultScroll = 0
		return m, nil
	}
	return m, nil
}

// handlePasswordKey collects the sudo password.
//
// The model's copy is cleared the moment the run is launched. Go strings are
// immutable so the bytes cannot be wiped in place; dropping the reference as
// early as possible is the practical limit, and the value never reaches disk,
// argv, or the rendered screen.
func (m Model) handlePasswordKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.view = viewTable
		m.password = ""
		m.prompt = ""
		m.asRoot = false
		return m, nil

	case tea.KeyCtrlC:
		m.quitting = true
		m.password = ""
		return m, tea.Quit

	case tea.KeyEnter:
		if m.password == "" {
			return m, nil
		}
		script, err := resolveScript(m.prompt)
		if err != nil {
			m.password = ""
			m.view = viewResults
			m.results = []execResult{{Host: "—", Err: err}}
			return m, nil
		}

		cmd := m.runOnTargets(script, m.targets())
		m.password = "" // the request already holds its own copy
		m.view = viewResults
		m.running = true
		m.results = nil
		m.resultScroll = 0
		return m, cmd

	case tea.KeyBackspace:
		if r := []rune(m.password); len(r) > 0 {
			m.password = string(r[:len(r)-1])
		}
		return m, nil

	case tea.KeyCtrlU:
		m.password = ""
		return m, nil

	case tea.KeySpace:
		m.password += " "
		return m, nil

	case tea.KeyRunes:
		m.password += string(msg.Runes)
		return m, nil
	}
	return m, nil
}
