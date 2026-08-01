package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/Sousf/hmon/internal/model"
)

// Column widths. The host column flexes with the longest name; the rest are
// fixed so numbers stay aligned down the screen.
const (
	colStatusW = 9
	colUpW     = 8
	colCPUW    = 5
	colSparkW  = 8
	colMemW    = 13
	colDiskW   = 6
	colTempW   = 6
	colNetW    = 17
)

// renderTable draws the fleet table, plus the live detail pane when there is
// room for it.
func (m Model) renderTable() string { return m.renderTableWith(true) }

// renderTableOnly draws just the table, used as the backdrop for the command
// prompt where the pane would only compete for attention with what you are
// typing.
func (m Model) renderTableOnly() string { return m.renderTableWith(false) }

func (m Model) renderTableWith(withPane bool) string {
	hosts := m.sortedHosts()
	prefixes := treePrefixes(hosts)

	nameW := 4
	for i, h := range hosts {
		if n := runewidth.StringWidth(prefixes[i] + h.Display() + kindTag(h)); n > nameW {
			nameW = n
		}
	}

	var b strings.Builder

	// Title bar.
	//
	// A stopped guest counts towards neither figure. It is not down, it is off
	// because someone turned it off, and folding it into the ratio would make
	// the fleet read as degraded every time a VM is deliberately parked.
	up, total := 0, 0
	for _, h := range hosts {
		if h.Status == model.StatusStopped {
			continue
		}
		total++
		if h.Status == model.StatusUp {
			up++
		}
	}
	title := styleTitle.Render("hmon")
	summary := styleDim.Render(fmt.Sprintf("%d/%d up · %s",
		up, total, m.now.Format("15:04:05")))
	b.WriteString(title + "  " + summary + "\n\n")

	// Header row. The leading pad matches the width of the selection cursor
	// each data row carries, without which every column header sits two
	// characters left of its values.
	b.WriteString("   ")
	b.WriteString(styleHeader.Render(
		padRight("HOST", nameW+healthFlagW) + "  " +
			padCentre("STATUS", colStatusW) + "  " +
			padLeft("UPTIME", colUpW) + "  " +
			padLeft("CPU", colCPUW) + " " +
			padRight("", colSparkW) + "  " +
			padRight("MEM", colMemW) + "  " +
			padLeft("DISK", colDiskW) + "  " +
			padLeft("TEMP", colTempW) + "  " +
			padRight("NET ↓ ↑", colNetW)))
	b.WriteString("\n")

	for i, h := range hosts {
		b.WriteString(m.renderRow(h, prefixes[i], nameW))
		b.WriteString("\n")
	}

	// Lines consumed so far: title, blank, header, one per host.
	used := 3 + len(hosts)

	// On a tall terminal the table alone leaves most of the screen empty, so
	// spend the remainder on live detail for the selected host. Budget is
	// whatever is left after reserving the separator, a blank, and the help
	// line.
	if budget := m.height - used - 3; withPane && m.splitActive() && budget >= minSplitLines {
		if h, ok := m.fleet.Get(m.selected); ok {
			b.WriteString(styleFaint.Render(separator(m.width)))
			b.WriteString("\n")
			for _, line := range m.detailPane(h, budget) {
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
	}

	if !withPane {
		return b.String()
	}

	b.WriteString("\n")
	b.WriteString(renderHelp(
		helpItem{"↑↓", "move"},
		helpItem{"enter", "detail"},
		helpItem{"space", "mark"},
		helpItem{"x", "run"},
		helpItem{"X", "run as root"},
		helpItem{"S", "ssh"},
		helpItem{"R", "reboot"},
		helpItem{"s", fmt.Sprintf("sort (%s%s)", m.sort, arrow(m.sortDesc))},
		helpItem{"q", "quit"},
	))
	return b.String()
}

// separator draws the rule between the table and the detail pane, capped so it
// does not stretch absurdly wide on a very large terminal.
func separator(width int) string {
	w := width - 2
	if w > 100 {
		w = 100
	}
	if w < 10 {
		w = 10
	}
	return " " + strings.Repeat("─", w)
}

func arrow(desc bool) string {
	if desc {
		return "↓"
	}
	return "↑"
}

// treePrefixes returns the branch drawing for each row, given rows already in
// display order — every guest immediately after its host. The last guest of a
// host gets the corner so the branch visibly ends there.
func treePrefixes(hosts []*model.Host) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		if !h.IsGuest() {
			continue
		}
		last := i+1 >= len(hosts) || hosts[i+1].Parent != h.Parent
		if last {
			out[i] = "└─ "
		} else {
			out[i] = "├─ "
		}
	}
	return out
}

// kindTag labels a guest as a virtual machine or a system container. Machines
// get nothing: the unlabelled rows are the ones you configured, and saying so
// on every one of them would be noise.
func kindTag(h *model.Host) string {
	switch h.Kind {
	case model.KindVM:
		return " vm"
	case model.KindContainer:
		return " ct"
	default:
		return ""
	}
}

// kindWord spells out what kindTag abbreviates, for the detail view where
// there is room for the whole word.
func kindWord(h *model.Host) string {
	if h.Kind == model.KindContainer {
		return "container"
	}
	return "virtual machine"
}

func (m Model) renderRow(h *model.Host, prefix string, nameW int) string {
	selected := h.Name == m.selected

	// Cursor and mark occupy separate cells: a host can be both the selection
	// and marked, and one glyph cannot say both.
	cursor := "  "
	if selected {
		cursor = styleSelected.Render("▸ ")
	}
	mark := " "
	if m.marked[h.Name] {
		// ASCII, for the same reason the health markers are: a font cannot draw
		// it wider than its cell.
		mark = styleAccentMark("*")
	}
	cursor += mark

	// The name cell is assembled from three differently coloured pieces, so it
	// is padded by measured width rather than by padRight — which would have to
	// measure the escape sequences too.
	tag := kindTag(h)
	label := truncate(h.Display(), nameW-runewidth.StringWidth(prefix+tag))
	name := styleFaint.Render(prefix)
	if selected {
		name += styleSelected.Render(label)
	} else {
		name += styleText.Render(label)
	}
	name += styleDim.Render(tag)
	if pad := nameW - runewidth.StringWidth(prefix+label+tag); pad > 0 {
		name += strings.Repeat(" ", pad)
	}
	// A failed unit is invisible in every resource column, so flag it against
	// the host name where it cannot be missed.
	name += healthFlag(h)

	// A host that is down has no current readings; showing stale numbers would
	// imply the machine is still reporting them.
	if !h.Status.Live() {
		return cursor + name + "  " +
			statusCell(h) + "  " +
			styleDim.Render(padLeft("—", colUpW)+"  ") +
			styleDim.Render(padLeft("—", colCPUW)+" "+padRight("", colSparkW)+"  "+
				padRight("—", colMemW)+"  "+
				padLeft("—", colDiskW)+"  "+
				padLeft("—", colTempW)+"  "+
				padRight("—", colNetW))
	}

	return cursor + name + "  " +
		statusCell(h) + "  " +
		uptimeCell(h) + "  " +
		m.cpuCell(h) + " " +
		styleDim.Render(sparkline(h.CPUHist.Values(), colSparkW, 100)) + "  " +
		m.memCell(h) + "  " +
		m.diskCell(h) + "  " +
		m.tempCell(h) + "  " +
		netCell(h)
}

// healthFlagW is the fixed width the flag occupies so every column to the
// right stays aligned whether or not a host has something to report.
const healthFlagW = 2

// Health markers.
//
// These are deliberately conservative. Unicode and go-runewidth both consider
// glyphs like ⟳ (U+27F3) one column wide, and the terminal advances the cursor
// accordingly — but plenty of monospace fonts draw them wider than their cell,
// which bleeds over the following character. No width calculation can fix
// that, because nothing is miscounted; the glyph is simply drawn too big. The
// only reliable defence is to stick to characters that are safe in any
// monospace font.
const (
	glyphFailed = "✗" // proven to render correctly at one cell
	glyphReboot = "!" // ASCII, so no font can draw it oversized
)

// healthFlag marks conditions no resource column can express, worst first: a
// watched service or container that is not running, a failed unit, or a
// pending reboot.
func healthFlag(h *model.Host) string {
	switch {
	case !h.Status.Live():
		return padRight("", healthFlagW)
	case len(h.Cur.StoppedServices()) > 0 || len(h.Cur.StoppedContainers()) > 0:
		// Something the operator explicitly said they care about is down, which
		// outranks a unit that merely failed at some point.
		return styleCrit.Render(padRight(" "+glyphFailed, healthFlagW))
	case len(h.Cur.FailedUnits) > 0:
		return styleCrit.Render(padRight(" "+glyphFailed, healthFlagW))
	case h.Cur.RebootRequired:
		return styleWarn.Render(padRight(" "+glyphReboot, healthFlagW))
	default:
		return padRight("", healthFlagW)
	}
}

// uptimeCell shows how long the host has been up, highlighting a machine that
// only just came back — which is what you want to see after pressing R.
func uptimeCell(h *model.Host) string {
	if h.Cur.Uptime <= 0 {
		return styleDim.Render(padLeft("—", colUpW))
	}
	txt := padLeft(humanDuration(h.Cur.Uptime), colUpW)
	if h.Cur.JustRebooted() {
		return styleWarn.Render(txt)
	}
	return styleDim.Render(txt)
}

// statusCell renders the status for the table, padded to a fixed column.
func statusCell(h *model.Host) string {
	// The column keeps its full width even when only a dot is drawn, so nothing
	// to its right shifts when a host changes state. A table whose columns move
	// the moment something goes wrong is hard to read at exactly the moment it
	// matters. Contents are centred so a lone dot sits in the column rather
	// than clinging to its left edge.
	return statusStyle(h).Render(padCentre(statusText(h), colStatusW))
}

// statusInline renders the same status without column padding, for the detail
// views where there is no column to line up with.
func statusInline(h *model.Host) string {
	return statusStyle(h).Render(statusText(h))
}

func statusText(h *model.Host) string {
	dot, label, _ := statusParts(h)
	if label == "" {
		return dot
	}
	return dot + " " + label
}

func statusStyle(h *model.Host) lipgloss.Style {
	_, _, st := statusParts(h)
	return st
}

func statusParts(h *model.Host) (dot, label string, st lipgloss.Style) {
	st = styleDim

	switch h.Status {
	case model.StatusUp:
		// A healthy host needs no word. The filled green dot says it, and
		// leaving the label off makes the rows that do carry text — down, auth,
		// bad output — stand out by being the only ones with any.
		//
		// Shape carries the meaning as well as colour: ● filled versus ○ hollow
		// stays legible without relying on the green being distinguishable.
		dot, label, st = "●", "", styleOK
	case model.StatusStale:
		dot, label, st = "◐", "stale", styleWarn
	case model.StatusStopped:
		// Only guests reach this, and it is not a fault: someone stopped it.
		// Drawn in the same quiet grey as the dashes filling the rest of the
		// row, so it reads as absence rather than as an alarm.
		dot, label, st = "○", "stopped", styleDim
	case model.StatusDown:
		dot, label, st = "○", "down", styleDim
	case model.StatusAuth:
		// Distinct from down on purpose: this one is a local configuration
		// problem, and retrying will never clear it.
		dot, label, st = "✗", "auth", styleCrit
	case model.StatusBadOutput:
		dot, label, st = "⚠", "bad out", styleCrit
	default:
		dot, label = "·", "—"
	}
	return dot, label, st
}

func (m Model) cpuCell(h *model.Host) string {
	if !h.HasCPUPct {
		// Needs two samples to exist; blank rather than a convincing zero.
		return styleDim.Render(padLeft("—", colCPUW))
	}
	txt := fmt.Sprintf("%.0f%%", h.CPUPct)
	return levelStyle(m.cfg.Thresholds.CPU.Classify(h.CPUPct)).Render(padLeft(txt, colCPUW))
}

func (m Model) memCell(h *model.Host) string {
	if !h.Cur.HasMem {
		return styleDim.Render(padRight("—", colMemW))
	}
	txt := fmt.Sprintf("%s/%s", humanBytes(h.Cur.MemUsed()), humanBytes(h.Cur.MemTotal))
	return levelStyle(m.cfg.Thresholds.Mem.Classify(h.Cur.MemPct())).Render(padRight(txt, colMemW))
}

func (m Model) diskCell(h *model.Host) string {
	fs, ok := h.Cur.RootFS()
	if !ok {
		return styleDim.Render(padLeft("—", colDiskW))
	}
	pct := fs.UsedPct()
	return levelStyle(m.cfg.Thresholds.Disk.Classify(pct)).
		Render(padLeft(fmt.Sprintf("%.0f%%", pct), colDiskW))
}

func (m Model) tempCell(h *model.Host) string {
	t, ok := h.Cur.MaxTemp()
	if !ok {
		// No exposed sensors is normal on plenty of hardware, so this is "not
		// available", not a failure.
		return styleDim.Render(padLeft("n/a", colTempW))
	}
	return levelStyle(m.cfg.Thresholds.Temp.Classify(t.C)).
		Render(padLeft(fmt.Sprintf("%.0f°C", t.C), colTempW))
}

func netCell(h *model.Host) string {
	if !h.HasNet {
		return styleDim.Render(padRight("—", colNetW))
	}
	rx, tx := h.TotalNet()
	return styleText.Render(padRight(
		fmt.Sprintf("↓%s ↑%s", humanRate(rx), humanRate(tx)), colNetW))
}

func styleAccentMark(s string) string { return styleWarn.Render(s) }
