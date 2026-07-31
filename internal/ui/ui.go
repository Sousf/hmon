// Package ui renders the fleet and handles input.
//
// All fleet state is mutated inside Update, which Bubble Tea runs on a single
// goroutine. Collection goroutines only ever send immutable messages inward,
// which is why nothing in this program needs a mutex.
package ui

import (
	"context"
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
)

// Poller is the collection dependency, narrowed to what the UI actually calls
// so tests can substitute a fake without touching SSH.
type Poller interface {
	Poll(ctx context.Context, addr string, withProcs bool) (model.Sample, error)
}

// Model is the Bubble Tea model.
type Model struct {
	cfg    *config.Config
	fleet  *model.Fleet
	poller Poller

	view     view
	selected string // tracked by host name, not index, so the cursor stays on
	// the same machine when the sort order changes
	sort     sortKey
	sortDesc bool
	procSort procSort

	width, height int
	now           time.Time

	// inFlight prevents a slow host from accumulating overlapping polls. The
	// config guarantees timeout < interval, so this is belt-and-braces.
	inFlight map[string]bool

	quitting bool
}

// New builds the UI model.
func New(cfg *config.Config, fleet *model.Fleet, poller Poller) Model {
	m := Model{
		cfg:      cfg,
		fleet:    fleet,
		poller:   poller,
		sort:     sortName,
		inFlight: make(map[string]bool),
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
		if m.inFlight[h.Name] {
			continue
		}
		// Processes are only collected for the host being viewed in detail:
		// they cost an extra ~0.5s sampling window on the remote side, and they
		// are not shown anywhere else.
		withProcs := m.view == viewDetail && h.Name == m.selected
		cmds = append(cmds, m.pollOne(h.Name, h.Addr, withProcs))
	}
	return tea.Batch(cmds...)
}

func (m Model) pollOne(name, addr string, withProcs bool) tea.Cmd {
	poller := m.poller
	return func() tea.Msg {
		s, err := poller.Poll(context.Background(), addr, withProcs)
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
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
			// Fetch processes for this host straight away rather than making
			// the user wait a full interval to see them.
			if h, ok := m.fleet.Get(m.selected); ok && !m.inFlight[h.Name] {
				m.inFlight[h.Name] = true
				return m, m.pollOne(h.Name, h.Addr, true)
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

	case "c":
		if m.view == viewDetail {
			m.procSort = procByCPU
		}
		return m, nil

	case "m":
		if m.view == viewDetail {
			m.procSort = procByMem
		}
		return m, nil
	}
	return m, nil
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

// sortedHosts returns hosts in display order. The sort is stable and always
// falls back to name, so equal values (every host at 0% CPU, say) do not
// reshuffle between frames.
func (m Model) sortedHosts() []*model.Host {
	hosts := make([]*model.Host, len(m.fleet.Hosts))
	copy(hosts, m.fleet.Hosts)

	sort.SliceStable(hosts, func(i, j int) bool {
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
			return a.Name < b.Name
		}
		if m.sortDesc {
			return !less
		}
		return less
	})
	return hosts
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
	if m.view == viewDetail {
		return m.renderDetail()
	}
	return m.renderTable()
}
