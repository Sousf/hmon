package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Sousf/hmon/internal/model"
)

const (
	detailSparkW = 24
	detailBarW   = 20

	// minSplitLines is the smallest detail pane worth drawing. Below this it
	// would show a header and little else, so the table simply keeps the space.
	minSplitLines = 8

	paneSparkW = 16
	paneBarW   = 14
)

// splitActive reports whether the terminal is tall enough to show live detail
// beneath the table. Height is zero until the first WindowSizeMsg arrives, so
// this is false for the first frame and the layout starts compact.
func (m Model) splitActive() bool {
	return m.view == viewTable && m.height > 0 &&
		m.height-(3+len(m.fleet.Hosts))-3 >= minSplitLines
}

// detailPane renders live detail for one host into at most budget lines.
//
// The fixed sections are laid out first and whatever remains goes to the
// process list, so a taller terminal shows more processes rather than the pane
// overflowing and pushing the help line off screen.
func (m Model) detailPane(h *model.Host, budget int) []string {
	if budget <= 0 {
		return nil
	}

	head := []string{
		"  " + styleTitle.Render(h.Name) + "  " + statusCell(h) +
			paneUptime(h),
	}

	if !h.Status.Live() {
		if h.LastErr != "" {
			head = append(head, "  "+styleCrit.Render(truncate(h.LastErr, maxInt(20, m.width-4))))
		}
		if !h.LastSeen.IsZero() {
			head = append(head, "  "+styleDim.Render("last seen "+h.LastSeen.Format("15:04:05")))
		}
		return clampLines(head, budget)
	}

	// Failed units go directly under the header: it is the one condition here
	// that means something is actually broken, so it should not be below the
	// fold on a short pane.
	if len(h.Cur.FailedUnits) > 0 {
		head = append(head, "")
		head = append(head, "  "+styleCrit.Render(glyphFailed+" failed: ")+
			styleText.Render(truncate(strings.Join(h.Cur.FailedUnits, ", "), maxInt(20, m.width-16))))
	}
	if h.Cur.RebootRequired {
		head = append(head, "  "+styleWarn.Render(glyphReboot+" reboot required"))
	}

	head = append(head, "")
	head = append(head, "  "+m.paneCPULine(h))
	head = append(head, "  "+m.paneMemLine(h))
	if swap := m.paneSwapLine(h); swap != "" {
		head = append(head, "  "+swap)
	}

	if len(h.Cur.FS) > 0 {
		head = append(head, "")
		for _, fs := range h.Cur.FS {
			head = append(head, "  "+m.paneFSLine(fs))
		}
	}

	if net := m.paneNetLine(h); net != "" {
		head = append(head, "")
		head = append(head, "  "+net)
	}
	if disk := m.paneDiskLine(h); disk != "" {
		head = append(head, "  "+disk)
	}

	if temps := m.paneTempLine(h); temps != "" {
		head = append(head, "  "+temps)
	}

	// Everything above is fixed; the process list absorbs the slack. Two lines
	// are reserved for its blank separator and column header.
	remaining := budget - len(head) - 2
	if remaining >= 1 && len(h.Cur.Procs) > 0 {
		head = append(head, "")
		head = append(head, "  "+styleHeader.Render(
			padLeft("PID", 8)+"  "+padLeft("CPU%", 6)+"  "+padLeft("RSS", 8)+"  COMMAND"))
		head = append(head, m.paneProcLines(h, remaining)...)
	} else if remaining >= 1 && len(h.Cur.Procs) == 0 {
		head = append(head, "")
		head = append(head, "  "+styleDim.Render("collecting processes…"))
	}

	return clampLines(head, budget)
}

func paneUptime(h *model.Host) string {
	if h.Status.Live() && h.Cur.Uptime > 0 {
		return styleDim.Render("  · up " + humanDuration(h.Cur.Uptime))
	}
	return ""
}

func (m Model) paneCPULine(h *model.Host) string {
	label := styleHeader.Render(padRight("CPU", 6))
	load := m.loadText(h)

	if !h.HasCPUPct {
		return label + styleDim.Render(padLeft("—", 6)) + "  " +
			padRight("", paneSparkW) + load
	}
	return label +
		levelStyle(m.cfg.Thresholds.CPU.Classify(h.CPUPct)).
			Render(padLeft(fmt.Sprintf("%.1f%%", h.CPUPct), 6)) + "  " +
		styleDim.Render(sparkline(h.CPUHist.Values(), paneSparkW, 100)) + load
}

// loadText renders the load average against core count. Load alone is not
// comparable between machines — 4.0 is idle on a 16-core box and a crisis on a
// dual-core Pi — so the per-core ratio is what gets coloured.
func (m Model) loadText(h *model.Host) string {
	raw := fmt.Sprintf("   LOAD  %.2f  %.2f  %.2f",
		h.Cur.Load[0], h.Cur.Load[1], h.Cur.Load[2])

	ratio, ok := h.Cur.LoadPerCore()
	if !ok {
		return styleDim.Render(raw)
	}
	// 1.0 per core means fully committed; warn approaching it and flag beyond.
	st := styleOK
	switch {
	case ratio >= 1.0:
		st = styleCrit
	case ratio >= 0.7:
		st = styleWarn
	}
	return styleDim.Render(raw) + "  " +
		st.Render(fmt.Sprintf("(%.0f%% of %d cores)", ratio*100, h.Cur.Cores))
}

func (m Model) paneSwapLine(h *model.Host) string {
	// No swap configured is normal and not worth a line.
	if h.Cur.SwapTotal == 0 {
		return ""
	}
	pct := h.Cur.SwapPct()
	// Swap thresholds are deliberately tighter than memory: any sustained swap
	// use on a homelab box is a symptom, not a steady state.
	st := styleOK
	switch {
	case pct >= 50:
		st = styleCrit
	case pct >= 10:
		st = styleWarn
	}
	return styleHeader.Render(padRight("SWAP", 6)) +
		st.Render(padLeft(fmt.Sprintf("%.0f%%", pct), 6)) + "  " +
		padRight("", paneSparkW) +
		styleText.Render(fmt.Sprintf("   %s / %s",
			humanBytes(h.Cur.SwapUsed()), humanBytes(h.Cur.SwapTotal)))
}

func (m Model) paneDiskLine(h *model.Host) string {
	if !h.HasDisk {
		return ""
	}
	parts := make([]string, 0, len(h.DiskRates))
	for _, d := range h.DiskRates {
		parts = append(parts, styleText.Render(d.Name)+" "+
			styleDim.Render(fmt.Sprintf("r%s w%s", humanRate(d.Read), humanRate(d.Write))))
	}
	return styleHeader.Render(padRight("DISK", 6)) + strings.Join(parts, "   ")
}

func (m Model) paneMemLine(h *model.Host) string {
	label := styleHeader.Render(padRight("MEM", 6))
	if !h.Cur.HasMem {
		return label + styleDim.Render("n/a")
	}
	pct := h.Cur.MemPct()
	return label +
		levelStyle(m.cfg.Thresholds.Mem.Classify(pct)).
			Render(padLeft(fmt.Sprintf("%.0f%%", pct), 6)) + "  " +
		styleDim.Render(sparkline(h.MemHist.Values(), paneSparkW, 100)) +
		styleText.Render(fmt.Sprintf("   %s / %s",
			humanBytes(h.Cur.MemUsed()), humanBytes(h.Cur.MemTotal)))
}

func (m Model) paneFSLine(fs model.FS) string {
	pct := fs.UsedPct()
	st := levelStyle(m.cfg.Thresholds.Disk.Classify(pct))
	return styleText.Render(padRight(truncate(fs.Mount, 14), 14)) +
		st.Render(padLeft(fmt.Sprintf("%.0f%%", pct), 5)) + "  " +
		st.Render(bar(pct, paneBarW)) + "  " +
		styleDim.Render(fmt.Sprintf("%s free of %s", humanKB(fs.AvailKB), humanKB(fs.TotalKB)))
}

func (m Model) paneNetLine(h *model.Host) string {
	if !h.HasNet {
		return ""
	}
	parts := make([]string, 0, len(h.NetRates))
	for _, r := range h.NetRates {
		parts = append(parts, styleText.Render(r.Name)+" "+
			styleDim.Render(fmt.Sprintf("↓%s ↑%s", humanRate(r.Rx), humanRate(r.Tx))))
	}
	return styleHeader.Render(padRight("NET", 6)) + strings.Join(parts, "   ")
}

// paneTempLine shows only the hottest few sensors: a machine with a sensor per
// core reports a dozen or more, which would crowd out the process list.
func (m Model) paneTempLine(h *model.Host) string {
	if len(h.Cur.Temps) == 0 {
		return ""
	}
	temps := make([]model.Temp, len(h.Cur.Temps))
	copy(temps, h.Cur.Temps)
	sort.SliceStable(temps, func(i, j int) bool { return temps[i].C > temps[j].C })
	if len(temps) > 3 {
		temps = temps[:3]
	}

	parts := make([]string, 0, len(temps))
	for _, t := range temps {
		parts = append(parts, styleDim.Render(truncate(t.Label, 12)+" ")+
			levelStyle(m.cfg.Thresholds.Temp.Classify(t.C)).Render(fmt.Sprintf("%.0f°C", t.C)))
	}
	return styleHeader.Render(padRight("TEMP", 6)) + strings.Join(parts, "   ")
}

func (m Model) paneProcLines(h *model.Host, max int) []string {
	procs := sortedProcs(h.Cur.Procs, m.procSort)
	if len(procs) > max {
		procs = procs[:max]
	}
	out := make([]string, 0, len(procs))
	for _, p := range procs {
		out = append(out, "  "+
			styleDim.Render(padLeft(fmt.Sprintf("%d", p.PID), 8))+"  "+
			levelStyle(m.cfg.Thresholds.CPU.Classify(p.CPUPct)).
				Render(padLeft(fmt.Sprintf("%.1f", p.CPUPct), 6))+"  "+
			styleText.Render(padLeft(humanKB(p.RSSKB), 8))+"  "+
			styleText.Render(truncate(p.Command, maxInt(10, m.width-30))))
	}
	return out
}

// sortedProcs orders by the current process sort. The collector sends the
// union of both rankings, so switching costs no round trip.
//
// Each ordering falls back to the other metric for ties. That matters most on
// an idle host, where every process reports 0.0% CPU: without the fallback the
// tie is broken by whatever order the collector emitted, and the list fills
// with zero-RSS kernel threads instead of the processes actually running.
func sortedProcs(in []model.Proc, by procSort) []model.Proc {
	procs := make([]model.Proc, len(in))
	copy(procs, in)
	sort.SliceStable(procs, func(i, j int) bool {
		a, b := procs[i], procs[j]
		if by == procByMem {
			if a.RSSKB != b.RSSKB {
				return a.RSSKB > b.RSSKB
			}
			return a.CPUPct > b.CPUPct
		}
		if a.CPUPct != b.CPUPct {
			return a.CPUPct > b.CPUPct
		}
		return a.RSSKB > b.RSSKB
	})
	return procs
}

func clampLines(lines []string, budget int) []string {
	if len(lines) > budget {
		return lines[:budget]
	}
	return lines
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m Model) renderDetail() string {
	h, ok := m.fleet.Get(m.selected)
	if !ok {
		return "no host selected\n"
	}

	var b strings.Builder

	// Header.
	b.WriteString(styleTitle.Render(h.Name))
	b.WriteString("  ")
	b.WriteString(statusCell(h))
	if h.Status.Live() && h.Cur.Uptime > 0 {
		b.WriteString(styleDim.Render("· up " + humanDuration(h.Cur.Uptime)))
	}
	b.WriteString("\n")
	if h.Addr != h.Name {
		b.WriteString(styleDim.Render("  " + h.Addr))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// An unreachable host has nothing to show but why, and when it was last
	// seen. Surfacing the error here means diagnosing a red row does not
	// require leaving the tool.
	if !h.Status.Live() {
		if h.LastErr != "" {
			b.WriteString(styleCrit.Render("  " + truncate(h.LastErr, 76)))
			b.WriteString("\n")
		}
		if !h.LastSeen.IsZero() {
			b.WriteString(styleDim.Render("  last seen " + h.LastSeen.Format("15:04:05")))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(styleHelp.Render("esc back · r refresh · q quit"))
		return b.String()
	}

	b.WriteString(m.detailCPU(h))
	b.WriteString(m.detailMem(h))
	b.WriteString(m.detailDisks(h))
	b.WriteString(m.detailTemps(h))
	b.WriteString(m.detailNet(h))
	b.WriteString(m.detailProcs(h))

	b.WriteString("\n")
	b.WriteString(styleHelp.Render("esc back · c/m sort procs · r refresh · q quit"))
	return b.String()
}

func (m Model) detailCPU(h *model.Host) string {
	var b strings.Builder
	label := styleHeader.Render(padRight("CPU", 8))

	if !h.HasCPUPct {
		b.WriteString("  " + label + styleDim.Render("—  (awaiting second sample)") + "\n")
	} else {
		val := levelStyle(m.cfg.Thresholds.CPU.Classify(h.CPUPct)).
			Render(padLeft(fmt.Sprintf("%.1f%%", h.CPUPct), 6))
		spark := styleDim.Render(sparkline(h.CPUHist.Values(), detailSparkW, 100))
		b.WriteString("  " + label + val + "  " + spark + "\n")
	}

	load := h.Cur.Load
	b.WriteString("  " + styleHeader.Render(padRight("LOAD", 8)) +
		styleText.Render(fmt.Sprintf("%.2f  %.2f  %.2f", load[0], load[1], load[2])) + "\n")
	return b.String()
}

func (m Model) detailMem(h *model.Host) string {
	if !h.Cur.HasMem {
		return "  " + styleHeader.Render(padRight("MEM", 8)) + styleDim.Render("n/a") + "\n"
	}
	pct := h.Cur.MemPct()
	val := levelStyle(m.cfg.Thresholds.Mem.Classify(pct)).Render(padLeft(fmt.Sprintf("%.0f%%", pct), 6))
	txt := styleText.Render(fmt.Sprintf("%s / %s",
		humanBytes(h.Cur.MemUsed()), humanBytes(h.Cur.MemTotal)))
	spark := styleDim.Render(sparkline(h.MemHist.Values(), detailSparkW, 100))
	return "  " + styleHeader.Render(padRight("MEM", 8)) + val + "  " + txt + "  " + spark + "\n"
}

func (m Model) detailDisks(h *model.Host) string {
	if len(h.Cur.FS) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")

	mountW := 6
	for _, fs := range h.Cur.FS {
		if n := len([]rune(fs.Mount)); n > mountW {
			mountW = n
		}
	}
	if mountW > 24 {
		mountW = 24
	}

	for _, fs := range h.Cur.FS {
		pct := fs.UsedPct()
		st := levelStyle(m.cfg.Thresholds.Disk.Classify(pct))
		b.WriteString("  " +
			styleText.Render(padRight(truncate(fs.Mount, mountW), mountW)) + "  " +
			st.Render(padLeft(fmt.Sprintf("%.0f%%", pct), 4)) + "  " +
			st.Render(bar(pct, detailBarW)) + "  " +
			styleDim.Render(fmt.Sprintf("%s free of %s",
				humanKB(fs.AvailKB), humanKB(fs.TotalKB))) + "\n")
	}
	return b.String()
}

func (m Model) detailTemps(h *model.Host) string {
	if len(h.Cur.Temps) == 0 {
		return ""
	}
	// Hottest first: that is the one you opened this view to look at.
	temps := make([]model.Temp, len(h.Cur.Temps))
	copy(temps, h.Cur.Temps)
	sort.SliceStable(temps, func(i, j int) bool { return temps[i].C > temps[j].C })

	// A machine with a CPU package sensor per core easily reports a dozen or
	// more, so wrap into fixed columns rather than one line running off the
	// side of the terminal.
	const perRow = 4

	var b strings.Builder
	b.WriteString("\n")
	for i, t := range temps {
		if i%perRow == 0 {
			if i == 0 {
				b.WriteString("  " + styleHeader.Render(padRight("TEMP", 8)))
			} else {
				b.WriteString("\n  " + padRight("", 8))
			}
		}
		st := levelStyle(m.cfg.Thresholds.Temp.Classify(t.C))
		cell := styleDim.Render(padRight(truncate(t.Label, 14), 15)) +
			st.Render(padLeft(fmt.Sprintf("%.0f°C", t.C), 6))
		b.WriteString(cell + "  ")
	}
	b.WriteString("\n")
	return b.String()
}

func (m Model) detailNet(h *model.Host) string {
	if !h.HasNet {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n")
	for _, r := range h.NetRates {
		b.WriteString("  " + styleHeader.Render(padRight("NET", 8)) +
			styleText.Render(padRight(r.Name, 10)) +
			styleText.Render(fmt.Sprintf("↓ %-10s ↑ %s", humanRate(r.Rx), humanRate(r.Tx))) + "\n")
	}
	return b.String()
}

func (m Model) detailProcs(h *model.Host) string {
	var b strings.Builder
	b.WriteString("\n")

	if len(h.Cur.Procs) == 0 {
		b.WriteString("  " + styleDim.Render("collecting processes…") + "\n")
		return b.String()
	}

	// Sorted locally rather than asking the host to re-rank: the collector
	// sends the union of both rankings precisely so this toggle costs no round
	// trip.
	procs := sortedProcs(h.Cur.Procs, m.procSort)
	if len(procs) > 10 {
		procs = procs[:10]
	}

	active := func(s procSort, label string) string {
		if m.procSort == s {
			return styleAccentLabel(label)
		}
		return styleDim.Render(label)
	}
	b.WriteString("  " + styleHeader.Render("TOP PROCESSES") + "   " +
		active(procByCPU, "[c] cpu") + "  " + active(procByMem, "[m] mem") + "\n")

	b.WriteString("  " + styleHeader.Render(
		padLeft("PID", 7)+"  "+padLeft("CPU%", 6)+"  "+padLeft("RSS", 8)+"  COMMAND") + "\n")

	for _, p := range procs {
		b.WriteString("  " +
			styleDim.Render(padLeft(fmt.Sprintf("%d", p.PID), 7)) + "  " +
			levelStyle(m.cfg.Thresholds.CPU.Classify(p.CPUPct)).
				Render(padLeft(fmt.Sprintf("%.1f", p.CPUPct), 6)) + "  " +
			styleText.Render(padLeft(humanKB(p.RSSKB), 8)) + "  " +
			styleText.Render(truncate(p.Command, 40)) + "\n")
	}
	return b.String()
}

func styleAccentLabel(s string) string { return styleSelected.Render(s) }
