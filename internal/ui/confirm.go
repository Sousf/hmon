package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderConfirm draws the reboot confirmation.
//
// It deliberately spells out the exact command that will run and names the
// host twice — in the title and in the command — because the one mistake this
// dialog exists to prevent is rebooting a machine you did not mean to select.
func (m Model) renderConfirm() string {
	host, ok := m.fleet.Get(m.confirmReboot)
	if !ok {
		return ""
	}

	cmd := fmt.Sprintf("ssh -t %s sudo systemctl reboot", host.Addr)

	var b strings.Builder
	b.WriteString(styleCrit.Render("Reboot " + host.Name + "?"))
	b.WriteString("\n\n")

	b.WriteString(styleDim.Render("Everything running on it will be interrupted."))
	b.WriteString("\n\n")
	b.WriteString(styleDim.Render("This will run:"))
	b.WriteString("\n  ")
	b.WriteString(styleText.Render(cmd))
	b.WriteString("\n\n")

	if host.Cur.RebootRequired {
		b.WriteString(styleWarn.Render(glyphReboot + " this host reports a reboot is required"))
		b.WriteString("\n\n")
	}
	if !host.Status.Live() {
		// Rebooting something already unreachable will not work, and is a
		// strong sign the wrong row is selected.
		b.WriteString(styleCrit.Render("⚠ this host is currently " + host.Status.String()))
		b.WriteString("\n\n")
	}

	b.WriteString(styleSelected.Render("y") + styleDim.Render(" confirm") +
		styleDim.Render("  ·  any other key cancels"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colCrit).
		Padding(1, 3).
		Render(b.String())

	// Centre it when the terminal size is known; fall back to plain output
	// before the first WindowSizeMsg so nothing renders at a nonsense size.
	if m.width > 0 && m.height > 0 {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
	}
	return box
}
