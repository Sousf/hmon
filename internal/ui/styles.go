package ui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/Sousf/hmon/internal/config"
)

// Palette. Colours are the only thing thresholds drive — there is no alerting
// behind them.
// Greys come in three weights rather than one.
//
// A single "dim" doing duty for column headers, secondary values, and
// separators forces it to be dim enough for the separators, which leaves the
// values unreadable — especially against a transparent background, where the
// terminal's own wallpaper shows through and eats the low end of the ramp.
//
// In the 232–255 ramp, higher is lighter. So a light theme wants LOW numbers
// (dark text on a pale background) and a dark theme wants HIGH ones. The
// earlier values had this backwards for the light case: colDim was 245, a pale
// grey, drawn on a pale background.
var (
	colText = lipgloss.AdaptiveColor{Light: "236", Dark: "252"} // values
	// colHeader labels columns: present enough to find, quiet enough not to
	// compete with the numbers underneath.
	colHeader = lipgloss.AdaptiveColor{Light: "238", Dark: "248"}
	// colDim is secondary information that still has to be read — units, "free
	// of", timestamps, sensor labels.
	colDim = lipgloss.AdaptiveColor{Light: "240", Dark: "246"}
	// colFaint is structure rather than content: rules and separators, which
	// should recede.
	colFaint = lipgloss.AdaptiveColor{Light: "247", Dark: "241"}

	colAccent = lipgloss.AdaptiveColor{Light: "31", Dark: "39"}
	colOK     = lipgloss.AdaptiveColor{Light: "28", Dark: "42"}
	colWarn   = lipgloss.AdaptiveColor{Light: "130", Dark: "214"}
	colCrit   = lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
)

var (
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styleHeader   = lipgloss.NewStyle().Bold(true).Foreground(colHeader)
	styleDim      = lipgloss.NewStyle().Foreground(colDim)
	styleFaint    = lipgloss.NewStyle().Foreground(colFaint)
	styleText     = lipgloss.NewStyle().Foreground(colText)
	styleOK       = lipgloss.NewStyle().Foreground(colOK)
	styleWarn     = lipgloss.NewStyle().Foreground(colWarn)
	styleCrit     = lipgloss.NewStyle().Foreground(colCrit)
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(colAccent)

	// The key bindings are the one piece of chrome you actually read, so they
	// are drawn at full contrast rather than in the dim colour used for
	// secondary data. Dim grey disappears entirely against a transparent
	// terminal background, which is exactly where a help line is least
	// guessable.
	styleHelpKey  = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styleHelpText = lipgloss.NewStyle().Foreground(colText)
	styleHelpSep  = lipgloss.NewStyle().Foreground(colFaint)
)

// levelStyle maps a threshold classification onto its colour.
func levelStyle(l config.Level) lipgloss.Style {
	switch l {
	case config.LevelCrit:
		return styleCrit
	case config.LevelWarn:
		return styleWarn
	default:
		return styleOK
	}
}
