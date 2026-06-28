// Package tui implements the Bubble Tea dashboard: a session list on the left
// and a detail pane on the right, updated live from the registry.
package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/exdis/agent-monitor/internal/model"
	"github.com/exdis/agent-monitor/internal/registry"
)

// snapshotMsg delivers a new registry snapshot into the Bubble Tea loop.
type snapshotMsg registry.Snapshot

type focus int

const (
	focusList focus = iota
	focusDetail
)

// Model is the root TUI model.
type Model struct {
	reg     *registry.Registry
	sub     <-chan registry.Snapshot
	unsub   func()
	apiAddr string

	width, height int

	sessions []model.Session
	cursor   int
	selected string // GlobalKey of selected session (kept stable across updates)
	focus    focus

	// filters
	filterSource string // "", "opencode", "copilot"
	filterState  string // "", "active", "idle", "ended", "stale"
	paused       bool
	showHelp     bool

	detailScroll int
}

// New creates the TUI model and subscribes to the registry.
func New(reg *registry.Registry, apiAddr string) *Model {
	sub, unsub := reg.Subscribe()
	return &Model{reg: reg, sub: sub, unsub: unsub, apiAddr: apiAddr}
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.waitSnapshot(), tea.EnterAltScreen)
}

// waitSnapshot blocks on the registry subscription channel as a tea.Cmd.
func (m *Model) waitSnapshot() tea.Cmd {
	return func() tea.Msg {
		snap, ok := <-m.sub
		if !ok {
			return nil
		}
		return snapshotMsg(snap)
	}
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case snapshotMsg:
		if !m.paused {
			m.applySnapshot(registry.Snapshot(msg))
		}
		return m, m.waitSnapshot()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.unsub()
		return m, tea.Quit
	case "?":
		m.showHelp = !m.showHelp
	case "tab":
		if m.focus == focusList {
			m.focus = focusDetail
		} else {
			m.focus = focusList
		}
	case "j", "down":
		if m.focus == focusDetail {
			m.detailScroll++
		} else {
			m.moveCursor(1)
		}
	case "k", "up":
		if m.focus == focusDetail {
			if m.detailScroll > 0 {
				m.detailScroll--
			}
		} else {
			m.moveCursor(-1)
		}
	case "g", "home":
		m.cursor = 0
		m.syncSelected()
	case "G", "end":
		m.cursor = len(m.visible()) - 1
		m.syncSelected()
	case "p":
		m.paused = !m.paused
	case "s":
		m.cycleSourceFilter()
	case "f":
		m.cycleStateFilter()
	case "esc":
		m.showHelp = false
		m.focus = focusList
	}
	return m, nil
}

func (m *Model) moveCursor(delta int) {
	vis := m.visible()
	if len(vis) == 0 {
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(vis) {
		m.cursor = len(vis) - 1
	}
	m.detailScroll = 0
	m.syncSelected()
}

// syncSelected records the GlobalKey under the cursor so selection survives
// list reordering on the next snapshot.
func (m *Model) syncSelected() {
	vis := m.visible()
	if m.cursor >= 0 && m.cursor < len(vis) {
		m.selected = vis[m.cursor].GlobalKey()
	}
}

// applySnapshot replaces the session list while keeping the cursor on the same
// session where possible.
func (m *Model) applySnapshot(snap registry.Snapshot) {
	m.sessions = snap.Sessions
	// Restore cursor to the previously selected session if still present.
	if m.selected != "" {
		vis := m.visible()
		for i, s := range vis {
			if s.GlobalKey() == m.selected {
				m.cursor = i
				return
			}
		}
	}
	// Clamp.
	if n := len(m.visible()); m.cursor >= n {
		m.cursor = max(0, n-1)
	}
	m.syncSelected()
}

// visible applies active filters to the session list.
func (m *Model) visible() []model.Session {
	if m.filterSource == "" && m.filterState == "" {
		return m.sessions
	}
	out := make([]model.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if m.filterSource != "" && string(s.Source) != m.filterSource {
			continue
		}
		if m.filterState != "" && string(s.State) != m.filterState {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (m *Model) cycleSourceFilter() {
	order := []string{"", "opencode", "copilot"}
	m.filterSource = next(order, m.filterSource)
	m.cursor = 0
	m.syncSelected()
}

func (m *Model) cycleStateFilter() {
	order := []string{"", "waiting", "active", "idle", "stale", "ended"}
	m.filterState = next(order, m.filterState)
	m.cursor = 0
	m.syncSelected()
}

func next(order []string, cur string) string {
	for i, v := range order {
		if v == cur {
			return order[(i+1)%len(order)]
		}
	}
	return order[0]
}

// selectedSession returns the session under the cursor, if any.
func (m *Model) selectedSession() (model.Session, bool) {
	vis := m.visible()
	if m.cursor >= 0 && m.cursor < len(vis) {
		return vis[m.cursor], true
	}
	return model.Session{}, false
}

// View implements tea.Model.
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "loading…"
	}
	if m.showHelp {
		return m.helpView()
	}

	title := m.titleBar()
	status := m.statusBar()

	bodyHeight := m.height - lipgloss.Height(title) - lipgloss.Height(status)
	if bodyHeight < 3 {
		bodyHeight = 3
	}

	listWidth := m.width * 48 / 100
	if listWidth < 30 {
		listWidth = min(30, m.width)
	}
	detailWidth := m.width - listWidth
	if detailWidth < 0 {
		detailWidth = 0
	}

	list := m.listView(listWidth, bodyHeight)
	detail := m.detailView(detailWidth, bodyHeight)

	body := lipgloss.JoinHorizontal(lipgloss.Top, list, detail)
	return lipgloss.JoinVertical(lipgloss.Left, title, body, status)
}

func (m *Model) titleBar() string {
	left := styleTitleBar.Render(" agent-monitor ")
	vis := m.visible()
	var wait, act, idle, end int
	for _, s := range vis {
		switch s.State {
		case model.StateWaiting:
			wait++
		case model.StateActive:
			act++
		case model.StateIdle:
			idle++
		case model.StateStale, model.StateEnded:
			end++
		}
	}
	gActive, sActive := stateGlyph("active")
	gIdle, sIdle := stateGlyph("idle")
	counts := "  "
	if wait > 0 {
		gWait, sWait := stateGlyph("waiting")
		counts += sWait.Render(gWait) + sWait.Render(" "+itoa(wait)+" waiting  ")
	}
	counts += sActive.Render(gActive) + styleText.Render(" "+itoa(act)+" active  ") +
		sIdle.Render(gIdle) + styleText.Render(" "+itoa(idle)+" idle  ") +
		styleMuted.Render("✓ "+itoa(end)+" done")

	right := ""
	if m.apiAddr != "" {
		right = styleFaint.Render("api http://" + m.apiAddr)
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(counts) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + counts + spaces(gap) + right
}

func (m *Model) statusBar() string {
	var parts []string
	parts = append(parts, "↑/↓ move", "tab pane", "s source", "f state", "p "+onOff(m.paused, "paused", "live"), "? help", "q quit")
	if m.filterSource != "" {
		parts = append(parts, "[src:"+m.filterSource+"]")
	}
	if m.filterState != "" {
		parts = append(parts, "[state:"+m.filterState+"]")
	}
	return styleStatusBar.Render(strings.Join(parts, "  ·  "))
}

func (m *Model) helpView() string {
	b := &strings.Builder{}
	title := stylePaneTitle.Render("agent-monitor — help")
	b.WriteString(title + "\n\n")
	lines := [][2]string{
		{"↑/k, ↓/j", "move selection (or scroll detail when focused)"},
		{"tab", "switch focus between list and detail"},
		{"g / G", "jump to first / last session"},
		{"s", "cycle source filter (all → opencode → copilot)"},
		{"f", "cycle state filter (all → waiting → active → idle → stale → ended)"},
		{"p", "pause/resume live updates"},
		{"?", "toggle this help"},
		{"esc", "close help / return to list"},
		{"q, ctrl+c", "quit"},
	}
	for _, l := range lines {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(colAccent).Bold(true).Render(pad(l[0], 12)))
		b.WriteString(styleText.Render(l[1]) + "\n")
	}
	b.WriteString("\n" + styleMuted.Render("Status API endpoints:") + "\n")
	for _, l := range []string{
		"GET /status            JSON snapshot",
		"GET /status?recent=1   include activity timelines",
		"GET /events            SSE live stream",
		"GET /healthz           health check",
	} {
		b.WriteString("  " + styleFaint.Render(l) + "\n")
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		stylePane.Padding(1, 2).Render(b.String()))
}

func onOff(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}

// Program builds a tea.Program wired to ctx cancellation.
func Program(ctx context.Context, reg *registry.Registry, apiAddr string) *tea.Program {
	m := New(reg, apiAddr)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithMouseCellMotion())
	return p
}
