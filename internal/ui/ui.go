// Package ui renders the fleet and handles input.
//
// All fleet state is mutated inside Update, which Bubble Tea runs on a single
// goroutine. Collection goroutines only ever send immutable messages inward,
// which is why nothing in this program needs a mutex.
package ui

import (
	"context"
	"os/exec"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sousf/hmon/internal/collect"
	"github.com/Sousf/hmon/internal/config"
	"github.com/Sousf/hmon/internal/model"
)

// view is which screen is showing.
type view int

const (
	viewTable view = iota
	viewDetail
	viewPrompt   // typing a command to run across hosts
	viewPassword // entering the sudo password for a root command
	viewResults  // showing what that command produced
	viewLaunch   // creating a new LXD instance from a template
)

// sortKey is the column the table is ordered by.
type sortKey int

const (
	sortName sortKey = iota
	sortStatus
	sortCPU
	sortMem
	sortDisk
	sortTemp
)

func (s sortKey) String() string {
	switch s {
	case sortStatus:
		return "status"
	case sortCPU:
		return "cpu"
	case sortMem:
		return "mem"
	case sortDisk:
		return "disk"
	case sortTemp:
		return "temp"
	default:
		return "name"
	}
}

// procSort is how the detail view's process list is ordered.
type procSort int

const (
	procByCPU procSort = iota
	procByMem
)

// Messages flowing back from collection goroutines.
type (
	tickMsg   time.Time
	sampleMsg struct {
		host   string
		sample model.Sample
	}
	failMsg struct {
		host string
		kind model.FailKind
		err  string
	}
	// resumedMsg arrives when an interactive ssh session hands the terminal
	// back, and triggers an immediate refresh so the table is not showing
	// values from before the session started.
	resumedMsg struct{}
)

// Poller is the collection dependency, narrowed to what the UI actually calls
// so tests can substitute a fake without touching SSH.
type Poller interface {
	Poll(ctx context.Context, addr string, opts collect.Opts) (model.Sample, error)
}

// Model is the Bubble Tea model.
type Model struct {
	cfg      *config.Config
	fleet    *model.Fleet
	poller   Poller
	executor Executor
	// launcher is separate from executor only for its deadline: creating an
	// instance may download an image and legitimately take minutes, while an
	// ad-hoc command that has not finished in a minute has usually hung.
	launcher Executor

	view     view
	selected string // tracked by host name, not index, so the cursor stays on
	// the same machine when the sort order changes
	sort     sortKey
	sortDesc bool
	procSort procSort

	// confirmReboot holds the host awaiting reboot confirmation, empty when no
	// dialog is showing. Rebooting is the only thing hmon does that changes a
	// machine rather than reading it, so it never happens on a single
	// keystroke.
	confirmReboot string

	// marked selects hosts for a fan-out command. Empty means the command
	// applies to the cursor's host alone.
	marked map[string]bool

	// prompt holds the command being typed; results and resultScroll hold what
	// the last one produced.
	prompt string
	// asRoot runs the pending command under sudo; password holds the secret
	// only between entry and the run that consumes it.
	asRoot   bool
	password string

	results      []execResult
	resultScroll int
	running      bool

	// launch holds the in-progress new-instance flow, reset each time n opens
	// it so an abandoned name cannot leak into the next launch.
	launch launchState

	width, height int
	now           time.Time

	// inFlight prevents a slow host from accumulating overlapping polls. A
	// timeout longer than the interval is allowed, so this is what actually
	// keeps a slow host from stacking up requests.
	inFlight map[string]bool

	quitting bool
}

// New builds the UI model.
func New(cfg *config.Config, fleet *model.Fleet, poller Poller, executor, launcher Executor) Model {
	m := Model{
		cfg:      cfg,
		fleet:    fleet,
		poller:   poller,
		executor: executor,
		launcher: launcher,
		sort:     sortName,
		inFlight: make(map[string]bool),
		marked:   make(map[string]bool),
		now:      time.Now(),
	}
	if len(fleet.Hosts) > 0 {
		m.selected = fleet.Hosts[0].Name
	}
	return m
}

// Init starts the poll loop and fires an immediate first collection so the
// table is not empty for a full interval on launch.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.tick(), m.pollAll())
}

func (m Model) tick() tea.Cmd {
	return tea.Tick(m.cfg.Interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// pollAll launches one collection per host. Each runs independently, so a slow
// host delays only its own row.
func (m Model) pollAll() tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.fleet.Hosts))
	for _, h := range m.fleet.Hosts {
		// Guests have no address of their own; they are measured through the
		// machine hosting them, on that machine's poll.
		if h.IsGuest() || m.inFlight[h.Name] {
			continue
		}
		// The expensive sections are only collected for the host whose detail is
		// actually on screen — the full detail view, or the split pane on a tall
		// terminal. They cost an extra ~0.5s sampling window remotely, so the
		// rest of the fleet never pays for them. Watched services are cheap
		// enough that every host gets them every poll.
		cmds = append(cmds, m.pollOne(h, m.showingProcs() && h.Name == m.selected))
	}
	return tea.Batch(cmds...)
}

func (m Model) pollOne(h *model.Host, detail bool) tea.Cmd {
	poller := m.poller
	name, addr := h.Name, h.Addr
	opts := collect.Opts{
		Detail:   detail,
		Services: h.Services,
		// A watch list makes containers a fleet-wide health signal, so every
		// host with one is asked every poll — not just the selected row.
		Containers: len(h.Containers) > 0,
		Guests:     m.cfg.GuestsEnabled(),
		// Processes cost a sampling window inside the guest as well, so they
		// follow the same rule hosts do: only the row on screen pays for them.
		GuestProcs: m.selectedGuestOn(h),
	}
	return func() tea.Msg {
		s, err := poller.Poll(context.Background(), addr, opts)
		if err != nil {
			return failMsg{host: name, kind: collect.Classify(err), err: err.Error()}
		}
		return sampleMsg{host: name, sample: s}
	}
}

// markInFlight records which hosts a poll round covers. It runs on the same
// goroutine as every other state change.
func (m *Model) markInFlight() {
	for _, h := range m.fleet.Hosts {
		m.inFlight[h.Name] = true
	}
}

// Update handles one message. This is the only place fleet state changes.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		m.now = time.Time(msg)
		cmd := m.pollAll()
		m.markInFlight()
		return m, tea.Batch(cmd, m.tick())

	case sampleMsg:
		delete(m.inFlight, msg.host)
		m.fleet.Apply(msg.host, msg.sample)
		return m, nil

	case failMsg:
		delete(m.inFlight, msg.host)
		m.fleet.Fail(msg.host, msg.kind, msg.err)
		return m, nil

	case execDoneMsg:
		m.running = false
		m.results = msg.results
		m.resultScroll = 0
		m.view = viewResults
		return m, nil

	case resumedMsg:
		// Whatever was on screen is now as old as the ssh session was long.
		cmd := m.pollAll()
		m.markInFlight()
		return m, cmd
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While a reboot is pending confirmation the dialog owns the keyboard, so
	// no normal binding can fire underneath it.
	if m.confirmReboot != "" {
		return m.handleConfirmKey(msg)
	}
	// The prompt likewise swallows everything, or typing "q" into a command
	// would quit instead.
	if m.view == viewPrompt {
		return m.handlePromptKey(msg)
	}
	if m.view == viewPassword {
		return m.handlePasswordKey(msg)
	}
	if m.view == viewResults {
		return m.handleResultsKey(msg)
	}
	if m.view == viewLaunch {
		return m.handleLaunchKey(msg)
	}

	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "esc":
		if m.view == viewDetail {
			m.view = viewTable
		}
		return m, nil

	case "enter":
		if m.view == viewTable {
			m.view = viewDetail
			// Fetch processes for this row straight away rather than making the
			// user wait a full interval to see them. A guest is collected
			// through its host, so that is the poll to bring forward — and it
			// wants the guest's processes, not the host's.
			if h, ok := m.fleet.Get(m.selected); ok {
				target := h
				if h.IsGuest() {
					target, ok = m.fleet.Get(h.Parent)
				}
				if ok && !m.inFlight[target.Name] {
					m.inFlight[target.Name] = true
					return m, m.pollOne(target, target == h)
				}
			}
		}
		return m, nil

	case "up", "k":
		m.move(-1)
		return m, nil

	case "down", "j":
		m.move(1)
		return m, nil

	case "s":
		m.sort = (m.sort + 1) % 6
		// Names read naturally ascending; metrics are interesting at the top.
		m.sortDesc = m.sort != sortName
		return m, nil

	case "i":
		m.sortDesc = !m.sortDesc
		return m, nil

	case "r":
		cmd := m.pollAll()
		m.markInFlight()
		return m, cmd

	case "S":
		// Hand the terminal to ssh and take it back when the shell exits.
		// Bubble Tea restores the alt screen and input mode around this, so a
		// crashed session cannot leave the terminal in a broken state.
		if h, ok := m.selectedMachine(); ok {
			return m, tea.ExecProcess(exec.Command("ssh", h.Addr), func(error) tea.Msg {
				// Errors are deliberately dropped: ssh has already printed
				// whatever went wrong to the terminal the user was just
				// looking at, and a non-zero exit is normal (Ctrl-D, remote
				// exit 1, dropped connection).
				return resumedMsg{}
			})
		}
		return m, nil

	case " ":
		// Marking is what turns a single-host action into a fan-out, so only
		// machines can be marked — everything a mark leads to runs over ssh.
		if _, ok := m.selectedMachine(); ok {
			if m.marked[m.selected] {
				delete(m.marked, m.selected)
			} else {
				m.marked[m.selected] = true
			}
		}
		return m, nil

	case "a":
		// All-or-nothing rather than invert: after pressing it you know exactly
		// what is marked without reading the table.
		if len(m.marked) == m.machineCount() {
			m.marked = make(map[string]bool)
		} else {
			for _, h := range m.fleet.Hosts {
				if !h.IsGuest() {
					m.marked[h.Name] = true
				}
			}
		}
		return m, nil

	case "x":
		if len(m.targets()) > 0 {
			m.view = viewPrompt
			m.prompt = ""
			m.asRoot = false
		}
		return m, nil

	case "X":
		// Same prompt, but the command will run under sudo and so needs a
		// password before anything is sent.
		if len(m.targets()) > 0 {
			m.view = viewPrompt
			m.prompt = ""
			m.asRoot = true
		}
		return m, nil

	case "R":
		// Only opens the dialog. Nothing reaches the machine until confirmed.
		// Guests are excluded: rebooting one means `lxc restart` on its host,
		// which is a different command with different consequences, and wiring
		// it in behind the same key would make R mean two things.
		if _, ok := m.selectedMachine(); ok {
			m.confirmReboot = m.selected
		}
		return m, nil

	case "n":
		// Nothing to offer without templates, and a picker with no entries is
		// worse than no picker at all. Guests are refused for the same reason
		// they refuse every other action: this runs over ssh to an address they
		// do not have.
		if h, ok := m.selectedMachine(); ok && len(m.cfg.Templates) > 0 {
			m.view = viewLaunch
			m.launch = launchState{host: h.Name}
		}
		return m, nil

	case "c":
		if m.showingProcs() {
			m.procSort = procByCPU
		}
		return m, nil

	case "m":
		if m.showingProcs() {
			m.procSort = procByMem
		}
		return m, nil
	}
	return m, nil
}

// handleConfirmKey resolves the reboot dialog. Only "y" proceeds; every other
// key cancels, so there is no way to confirm by mashing.
func (m Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	host := m.confirmReboot
	m.confirmReboot = ""

	if msg.String() != "y" {
		return m, nil
	}

	h, ok := m.fleet.Get(host)
	if !ok {
		return m, nil
	}

	// Run interactively rather than in the background. The SSH user needs
	// sudo, and a backgrounded BatchMode call would fail invisibly on a
	// password prompt; this way sudo can ask, and any error is printed on the
	// terminal the user is already looking at.
	//
	// A non-zero exit is the normal case: the connection drops as the machine
	// goes down, so ssh reports failure for a reboot that worked.
	return m, tea.ExecProcess(
		exec.Command("ssh", "-t", h.Addr, "sudo", "systemctl", "reboot"),
		func(error) tea.Msg { return resumedMsg{} },
	)
}

// selectedMachine returns the selected row only when it is a real machine.
// Everything hmon can do beyond looking — ssh, fan-out commands, reboot — goes
// over ssh to an address, and a guest has none.
func (m Model) selectedMachine() (*model.Host, bool) {
	h, ok := m.fleet.Get(m.selected)
	if !ok || h.IsGuest() {
		return nil, false
	}
	return h, true
}

// machineCount is how many rows can be marked.
func (m Model) machineCount() int {
	n := 0
	for _, h := range m.fleet.Hosts {
		if !h.IsGuest() {
			n++
		}
	}
	return n
}

// selectedGuestOn returns the instance name to collect processes for when the
// selected row is a guest of this host, and empty otherwise.
func (m Model) selectedGuestOn(h *model.Host) string {
	if !m.showingProcs() {
		return ""
	}
	sel, ok := m.fleet.Get(m.selected)
	if !ok || sel.Parent != h.Name {
		return ""
	}
	return sel.Display()
}

// move shifts the selection by delta within the current sort order, clamping
// at the ends rather than wrapping.
func (m *Model) move(delta int) {
	hosts := m.sortedHosts()
	if len(hosts) == 0 {
		return
	}
	idx := 0
	for i, h := range hosts {
		if h.Name == m.selected {
			idx = i
			break
		}
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(hosts) {
		idx = len(hosts) - 1
	}
	m.selected = hosts[idx].Name
}

// sortedHosts returns rows in display order: configured machines ordered by the
// chosen column, each followed by the guests running on it. The sort is stable
// and always falls back to name, so equal values (every host at 0% CPU, say) do
// not reshuffle between frames.
//
// Guests are ordered by name under their own host rather than joining the
// global ordering. Sorting them into it would break the tree apart — a VM would
// drift away from the machine it runs on the moment it got busy, which is
// exactly when you want to see the two together.
func (m Model) sortedHosts() []*model.Host {
	var machines []*model.Host
	guests := make(map[string][]*model.Host)
	for _, h := range m.fleet.Hosts {
		if h.IsGuest() {
			guests[h.Parent] = append(guests[h.Parent], h)
		} else {
			machines = append(machines, h)
		}
	}

	sort.SliceStable(machines, m.less(machines))

	out := make([]*model.Host, 0, len(m.fleet.Hosts))
	for _, h := range machines {
		out = append(out, h)
		kids := guests[h.Name]
		sort.SliceStable(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
		out = append(out, kids...)
	}
	return out
}

// less builds the comparator for the chosen sort column.
func (m Model) less(hosts []*model.Host) func(i, j int) bool {
	return func(i, j int) bool {
		a, b := hosts[i], hosts[j]
		var less bool
		switch m.sort {
		case sortStatus:
			if a.Status == b.Status {
				return a.Name < b.Name
			}
			less = a.Status < b.Status
		case sortCPU:
			if a.CPUPct == b.CPUPct {
				return a.Name < b.Name
			}
			less = a.CPUPct < b.CPUPct
		case sortMem:
			if a.Cur.MemPct() == b.Cur.MemPct() {
				return a.Name < b.Name
			}
			less = a.Cur.MemPct() < b.Cur.MemPct()
		case sortDisk:
			less = diskPct(a) < diskPct(b)
			if diskPct(a) == diskPct(b) {
				return a.Name < b.Name
			}
		case sortTemp:
			less = maxTemp(a) < maxTemp(b)
			if maxTemp(a) == maxTemp(b) {
				return a.Name < b.Name
			}
		default:
			// Falls through to the flip below rather than returning here. The
			// tie-break branches above do return early on purpose — equal values
			// must always order by name, or rows reshuffle between frames — but
			// this is the name column itself, and i has to be able to reverse it.
			less = a.Name < b.Name
		}
		if m.sortDesc {
			return !less
		}
		return less
	}
}

func diskPct(h *model.Host) float64 {
	fs, ok := h.Cur.RootFS()
	if !ok {
		return -1
	}
	return fs.UsedPct()
}

func maxTemp(h *model.Host) float64 {
	t, ok := h.Cur.MaxTemp()
	if !ok {
		return -1
	}
	return t.C
}

// View renders the current screen.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	// The confirmation takes over the screen rather than overlaying the table.
	// Rebooting a machine should not look like an incidental prompt tucked
	// into a corner.
	if m.confirmReboot != "" {
		return m.renderConfirm()
	}
	switch m.view {
	case viewLaunch:
		return m.renderLaunch()
	case viewDetail:
		return m.renderDetail()
	case viewPrompt:
		return m.renderPrompt()
	case viewPassword:
		return m.renderPassword()
	case viewResults:
		return m.renderResults()
	}
	return m.renderTable()
}

// showingProcs reports whether a process list is currently on screen, which is
// what decides both whether to collect processes and whether the sort keys do
// anything.
func (m Model) showingProcs() bool {
	return m.view == viewDetail || m.splitActive()
}
