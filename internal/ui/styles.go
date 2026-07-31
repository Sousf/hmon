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
	styleHelp     = lipgloss.NewStyle().Foreground(colDim)
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
