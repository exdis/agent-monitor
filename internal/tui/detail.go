package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/exdis/agent-monitor/internal/model"
)

// detailView renders the detail pane for the selected session.
func (m *Model) detailView(width, height int) string {
	inner := width - 2
	if inner < 10 {
		inner = 10
	}
	contentHeight := height - 2
	if contentHeight < 1 {
		contentHeight = 1
	}

	border := stylePane
	if m.focus == focusDetail {
		border = border.BorderForeground(colAccent)
	}

	s, ok := m.selectedSession()
	if !ok {
		return border.Width(inner).Height(contentHeight).MaxHeight(height).
			Render(styleMuted.Render("Select a session to see details."))
	}

	var b strings.Builder

	title := s.Title
	if title == "" {
		title = s.ID
	}
	glyph, gStyle := stateGlyph(string(s.State))
	b.WriteString(stylePaneTitle.Render(truncate(title, inner)) + "\n")
	b.WriteString(gStyle.Render(glyph+" "+string(s.State)) + styleFaint.Render("  ·  "+string(s.Source)+"  ·  "+s.ID) + "\n")
	b.WriteString(strings.Repeat("─", inner) + "\n")

	// Prominent "waiting for you" callout.
	if s.State == model.StateWaiting && s.Waiting != nil {
		writeWaitBlock(&b, inner, s.Waiting)
	}

	// Metadata grid.
	writeField(&b, inner, "dir", s.Directory)
	if s.Branch != "" {
		writeField(&b, inner, "branch", s.Branch)
	}
	if s.Agent != "" {
		writeField(&b, inner, "agent", s.Agent)
	}
	if s.Model != "" {
		writeField(&b, inner, "model", s.Model)
	}
	if s.CurrentAction != "" && s.State == model.StateActive {
		writeField(&b, inner, "doing", s.CurrentAction)
	}
	if s.Tokens.Total() > 0 {
		tok := fmt.Sprintf("in %s · out %s", humanTokens(s.Tokens.Input), humanTokens(s.Tokens.Output))
		if s.Tokens.CacheRead > 0 || s.Tokens.CacheWrite > 0 {
			tok += fmt.Sprintf(" · cache r%s w%s", humanTokens(s.Tokens.CacheRead), humanTokens(s.Tokens.CacheWrite))
		}
		writeField(&b, inner, "tokens", tok)
	}
	if s.Cost > 0 {
		writeField(&b, inner, "cost", fmt.Sprintf("$%.4f", s.Cost))
	}
	writeField(&b, inner, "updated", humanAgo(s.LastEventAt)+" ago")

	// Activity feed.
	b.WriteString("\n" + stylePaneTitle.Render("Activity") + "\n")

	feed := m.renderActivity(s.Recent, inner)
	// Determine how many feed lines fit; allow scrolling within the feed.
	headerLines := strings.Count(b.String(), "\n")
	avail := contentHeight - headerLines
	if avail < 1 {
		avail = 1
	}
	feedLines := strings.Split(feed, "\n")
	// Default: show the tail (most recent). detailScroll moves the window up.
	if len(feedLines) > avail {
		end := len(feedLines) - m.detailScroll
		if end < avail {
			end = avail
		}
		if end > len(feedLines) {
			end = len(feedLines)
		}
		start := end - avail
		if start < 0 {
			start = 0
		}
		feedLines = feedLines[start:end]
	}
	b.WriteString(strings.Join(feedLines, "\n"))

	return border.Width(inner).Height(contentHeight).MaxHeight(height).Render(b.String())
}

func (m *Model) renderActivity(items []model.Activity, width int) string {
	if len(items) == 0 {
		return styleFaint.Render("No recorded activity yet.")
	}
	var lines []string
	for _, a := range items {
		ts := styleFaint.Render(a.At.Format("15:04:05"))
		var body string
		switch a.Kind {
		case model.ActivityTool:
			tool := lipgloss.NewStyle().Foreground(colAccent).Render(a.Tool)
			body = "⚙ " + tool
			if a.Text != "" {
				body += styleMuted.Render(" " + a.Text)
			}
		case model.ActivityMessage:
			if a.Role == model.RoleUser {
				body = lipgloss.NewStyle().Foreground(colIdle).Render("▶ you ") + styleText.Render(a.Text)
			} else {
				body = lipgloss.NewStyle().Foreground(colActive).Render("🤖 ") + styleText.Render(a.Text)
			}
		case model.ActivityTurn:
			body = styleMuted.Render("… " + a.Text)
		case model.ActivityError:
			body = lipgloss.NewStyle().Foreground(colStale).Render("⚠ " + a.Text)
		default:
			body = styleMuted.Render(a.Text)
		}
		prefix := ts + " "
		line := prefix + truncate(stripNewlines(body), width-lipgloss.Width(prefix))
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func writeField(b *strings.Builder, width int, label, value string) {
	if value == "" {
		return
	}
	l := lipgloss.NewStyle().Foreground(colFaint).Render(pad(label, 8))
	b.WriteString(l + " " + styleText.Render(truncate(value, width-9)) + "\n")
}

// writeWaitBlock renders the prominent "waiting for you" callout with the
// question/permission prompt and any options.
func writeWaitBlock(b *strings.Builder, width int, w *model.Wait) {
	hdr := lipgloss.NewStyle().Foreground(colWaiting).Bold(true)
	var label string
	switch w.Kind {
	case model.WaitQuestion:
		label = "⏳ WAITING FOR YOUR ANSWER"
	case model.WaitPermission:
		label = "⏳ WAITING FOR PERMISSION"
	case model.WaitApproval:
		label = "⏳ WAITING FOR APPROVAL"
	default:
		label = "⏳ WAITING FOR YOU"
	}
	if !w.Since.IsZero() {
		label += "  (" + humanAgo(w.Since) + ")"
	}
	b.WriteString(hdr.Render(truncate(label, width)) + "\n")
	if w.Prompt != "" {
		for _, ln := range wrapText(w.Prompt, width) {
			b.WriteString(styleText.Render(ln) + "\n")
		}
	}
	for i, opt := range w.Options {
		bullet := lipgloss.NewStyle().Foreground(colWaiting).Render("  " + romanOrNum(i) + ". ")
		b.WriteString(bullet + styleMuted.Render(truncate(opt, width-6)) + "\n")
	}
	b.WriteString(strings.Repeat("─", width) + "\n")
}

func romanOrNum(i int) string {
	return fmt.Sprintf("%d", i+1)
}

// wrapText word-wraps s to the given width, returning up to a few lines.
func wrapText(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	words := strings.Fields(stripNewlines(s))
	var lines []string
	cur := ""
	for _, w := range words {
		if cur == "" {
			cur = w
		} else if len([]rune(cur))+1+len([]rune(w)) <= width {
			cur += " " + w
		} else {
			lines = append(lines, cur)
			cur = w
		}
		if len(lines) >= 4 { // cap height
			break
		}
	}
	if cur != "" && len(lines) < 5 {
		lines = append(lines, cur)
	}
	return lines
}

func stripNewlines(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}
