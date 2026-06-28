package tui

import "github.com/charmbracelet/lipgloss"

// Color palette (256-color-safe).
var (
	colActive   = lipgloss.Color("42")  // green
	colWaiting  = lipgloss.Color("213") // bright magenta/pink - needs attention
	colIdle     = lipgloss.Color("220") // yellow
	colStale    = lipgloss.Color("208") // orange
	colEnded    = lipgloss.Color("245") // grey
	colAccent   = lipgloss.Color("39")  // blue
	colMuted    = lipgloss.Color("244")
	colFaint    = lipgloss.Color("240")
	colText     = lipgloss.Color("252")
	colSelBg    = lipgloss.Color("236")
	colHeaderBg = lipgloss.Color("237")
)

var (
	styleApp = lipgloss.NewStyle()

	styleTitleBar = lipgloss.NewStyle().
			Foreground(lipgloss.Color("231")).
			Background(colAccent).
			Bold(true).
			Padding(0, 1)

	styleStatusBar = lipgloss.NewStyle().
			Foreground(colMuted).
			Padding(0, 1)

	stylePane = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colFaint)

	stylePaneTitle = lipgloss.NewStyle().
			Foreground(colAccent).
			Bold(true)

	styleSelected = lipgloss.NewStyle().
			Background(colSelBg).
			Bold(true)

	styleMuted = lipgloss.NewStyle().Foreground(colMuted)
	styleFaint = lipgloss.NewStyle().Foreground(colFaint)
	styleText  = lipgloss.NewStyle().Foreground(colText)

	styleHelp = lipgloss.NewStyle().Foreground(colFaint)
)

// stateStyle returns the dot glyph and style for a session state.
func stateGlyph(state string) (string, lipgloss.Style) {
	switch state {
	case "active":
		return "●", lipgloss.NewStyle().Foreground(colActive)
	case "waiting":
		return "?", lipgloss.NewStyle().Foreground(colWaiting).Bold(true)
	case "idle":
		return "○", lipgloss.NewStyle().Foreground(colIdle)
	case "stale":
		return "◌", lipgloss.NewStyle().Foreground(colStale)
	case "ended":
		return "✓", lipgloss.NewStyle().Foreground(colEnded)
	default:
		return "·", styleMuted
	}
}

func sourceBadge(source string) string {
	switch source {
	case "opencode":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Render("OC")
	case "copilot":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Render("CP")
	default:
		return lipgloss.NewStyle().Foreground(colMuted).Render("??")
	}
}
