package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/exdis/agent-monitor/internal/model"
)

// listView renders the session summary list within the given dimensions.
func (m *Model) listView(width, height int) string {
	inner := width - 2 // borders
	if inner < 10 {
		inner = 10
	}
	contentHeight := height - 2 // borders
	if contentHeight < 1 {
		contentHeight = 1
	}

	vis := m.visible()
	header := stylePaneTitle.Render("Sessions") + styleFaint.Render("  ("+itoa(len(vis))+")")

	var rows []string
	rows = append(rows, header)

	if len(vis) == 0 {
		rows = append(rows, "")
		rows = append(rows, styleMuted.Render(truncate("No active or recent agent sessions.", inner)))
		rows = append(rows, styleFaint.Render(truncate("Start opencode or copilot in a terminal.", inner)))
	} else {
		// Scroll window so the cursor stays visible.
		bodyRows := contentHeight - 1 // minus header
		start := 0
		if m.cursor >= bodyRows {
			start = m.cursor - bodyRows + 1
		}
		end := min(len(vis), start+bodyRows)
		for i := start; i < end; i++ {
			rows = append(rows, m.renderRow(vis[i], i == m.cursor, inner))
		}
	}

	content := strings.Join(rows, "\n")
	border := stylePane
	if m.focus == focusList {
		border = border.BorderForeground(colAccent)
	}
	return border.Width(inner).Height(contentHeight).MaxHeight(height).Render(content)
}

// renderRow renders one session as two lines: a header line and a detail line.
func (m *Model) renderRow(s model.Session, selected bool, width int) string {
	glyph, gStyle := stateGlyph(string(s.State))
	badge := sourceBadge(string(s.Source))

	title := s.Title
	if title == "" {
		title = s.ID
	}

	// Line 1: ● OC  title.......................  3s
	ago := humanAgo(s.LastEventAt)
	left := gStyle.Render(glyph) + " " + badge + " "
	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(ago) + 1
	titleW := width - leftW - rightW
	if titleW < 4 {
		titleW = 4
	}
	titleStyled := styleText.Render(pad(title, titleW))
	if selected {
		titleStyled = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Bold(true).Render(pad(title, titleW))
	}
	line1 := left + titleStyled + " " + styleFaint.Render(ago)

	// Line 2: dim  dir · model · action / tokens — or the pending question when
	// the session is waiting on the user.
	var line2 string
	if s.State == model.StateWaiting && s.Waiting != nil {
		label := waitLabel(s.Waiting.Kind)
		prompt := s.Waiting.Prompt
		if prompt == "" {
			prompt = "needs your input"
		}
		txt := lipgloss.NewStyle().Foreground(colWaiting).Bold(true).Render(label) +
			lipgloss.NewStyle().Foreground(colWaiting).Render(" "+prompt)
		line2 = "   " + truncate(stripNL(txt), width-3)
	} else {
		var meta []string
		if d := shortDir(s.Directory); d != "" {
			meta = append(meta, d)
		}
		if s.Model != "" {
			meta = append(meta, modelShort(s.Model))
		}
		detail := strings.Join(meta, " · ")
		action := s.CurrentAction
		if action != "" && s.State == model.StateActive {
			detail = detail + " · " + action
		} else if s.Tokens.Total() > 0 {
			detail = detail + " · " + humanTokens(s.Tokens.Total()) + "tok"
		}
		line2 = "   " + styleMuted.Render(truncate(detail, width-3))
	}

	block := line1 + "\n" + line2
	if selected {
		block = lipgloss.NewStyle().Background(colSelBg).Width(width).Render(block)
	}
	return block
}

// waitLabel returns a short prefix for a wait kind.
func waitLabel(k model.WaitKind) string {
	switch k {
	case model.WaitQuestion:
		return "ASK"
	case model.WaitPermission:
		return "PERM"
	case model.WaitApproval:
		return "APPROVE"
	default:
		return "WAIT"
	}
}

func stripNL(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "\r", "")
}

func modelShort(s string) string {
	// Drop provider prefixes like "github-copilot/claude-..." → keep tail.
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return s
}
