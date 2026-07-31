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
)

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

	procs := make([]model.Proc, len(h.Cur.Procs))
	copy(procs, h.Cur.Procs)
	// Sort locally rather than asking the host to re-rank: the collector sends
	// the union of both rankings precisely so this toggle costs no round trip.
	sort.SliceStable(procs, func(i, j int) bool {
		if m.procSort == procByMem {
			return procs[i].RSSKB > procs[j].RSSKB
		}
		return procs[i].CPUPct > procs[j].CPUPct
	})
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
