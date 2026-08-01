package ui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/Sousf/hmon/internal/config"
)

// Palette. Colours are the only thing thresholds drive — there is no alerting
// behind them.
var (
	colDim    = lipgloss.AdaptiveColor{Light: "245", Dark: "240"}
	colText   = lipgloss.AdaptiveColor{Light: "236", Dark: "252"}
	colAccent = lipgloss.AdaptiveColor{Light: "31", Dark: "39"}
	colOK     = lipgloss.AdaptiveColor{Light: "28", Dark: "42"}
	colWarn   = lipgloss.AdaptiveColor{Light: "130", Dark: "214"}
	colCrit   = lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
)

var (
	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	styleHeader   = lipgloss.NewStyle().Bold(true).Foreground(colDim)
	styleDim      = lipgloss.NewStyle().Foreground(colDim)
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
	styleHelpSep  = lipgloss.NewStyle().Foreground(colDim)
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
