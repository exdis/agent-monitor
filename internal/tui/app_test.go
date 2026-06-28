package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/exdis/agent-monitor/internal/model"
	"github.com/exdis/agent-monitor/internal/registry"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

// deANSI strips terminal escape sequences so substring assertions are stable.
func deANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// newTestModel builds a Model without subscribing to a live registry.
func newTestModel() *Model {
	return &Model{apiAddr: "127.0.0.1:7654"}
}

func sampleSessions() []model.Session {
	now := time.Now()
	return []model.Session{
		{
			ID: "ses_active", Source: model.SourceOpenCode,
			Title: "Implementing the parser", Directory: "/home/u/dev/proj",
			Agent: "build", Model: "github-copilot/claude-opus-4.8",
			State: model.StateActive, CurrentAction: "running bash: go test ./...",
			Tokens:      model.Tokens{Input: 1200, Output: 800, CacheRead: 64000},
			Cost:        0.12,
			LastEventAt: now.Add(-3 * time.Second),
			Recent: []model.Activity{
				{Kind: model.ActivityMessage, Role: model.RoleUser, Text: "fix the bug", At: now.Add(-30 * time.Second)},
				{Kind: model.ActivityTool, Tool: "bash", Text: "go test ./...", At: now.Add(-3 * time.Second)},
			},
		},
		{
			ID: "cp_idle", Source: model.SourceCopilot,
			Title: "analyze code", Directory: "/home/u/dev/zig", Branch: "main",
			Model: "claude-opus-4.6", State: model.StateIdle,
			LastEventAt: now.Add(-2 * time.Minute),
		},
	}
}

func TestViewRendersSessions(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 130, 40
	m.applySnapshot(registry.Snapshot{Sessions: sampleSessions()})

	out := deANSI(m.View())
	for _, want := range []string{
		"agent-monitor", // title bar
		"Sessions",      // list header
		"Implementing the parser",
		"analyze code",
		"Activity",      // detail pane
		"go test ./...", // current action / activity
		"api http://127.0.0.1:7654",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("View() missing %q\n---\n%s", want, out)
		}
	}
}

func TestFiltersAndNavigation(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 130, 40
	m.applySnapshot(registry.Snapshot{Sessions: sampleSessions()})

	// Source filter: opencode only.
	m.filterSource = "opencode"
	if got := len(m.visible()); got != 1 {
		t.Fatalf("opencode filter => %d sessions, want 1", got)
	}
	if s, _ := m.selectedSession(); s.Source != model.SourceOpenCode {
		t.Fatalf("selected source = %s, want opencode", s.Source)
	}

	// Cycle state filter through the full ring back to empty.
	m.filterSource = ""
	for i := 0; i < 6; i++ {
		m.cycleStateFilter()
	}
	if m.filterState != "" {
		t.Fatalf("state filter ring did not return to empty, got %q", m.filterState)
	}

	// Navigation clamps.
	m.moveCursor(100)
	if m.cursor != len(m.visible())-1 {
		t.Fatalf("cursor not clamped to last, got %d", m.cursor)
	}
	m.moveCursor(-100)
	if m.cursor != 0 {
		t.Fatalf("cursor not clamped to first, got %d", m.cursor)
	}
}

func TestSelectionSurvivesReorder(t *testing.T) {
	m := newTestModel()
	m.width, m.height = 130, 40
	ss := sampleSessions()
	m.applySnapshot(registry.Snapshot{Sessions: ss})
	m.moveCursor(1) // select the copilot session
	sel := m.selected

	// Reorder: put copilot first.
	reordered := []model.Session{ss[1], ss[0]}
	m.applySnapshot(registry.Snapshot{Sessions: reordered})

	if got := m.selected; got != sel {
		t.Fatalf("selection changed across reorder: got %q want %q", got, sel)
	}
	if s, _ := m.selectedSession(); s.ID != "cp_idle" {
		t.Fatalf("selected session = %s, want cp_idle", s.ID)
	}
}

func TestKeyQuitReturnsQuitCmd(t *testing.T) {
	m := newTestModel()
	m.unsub = func() {}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
}

func TestWaitingRendersQuestion(t *testing.T) {
	now := time.Now()
	waiting := model.Session{
		ID: "ses_wait", Source: model.SourceOpenCode,
		Title: "Configuring wallpaper", Directory: "/home/u/.nixos",
		State:       model.StateWaiting,
		LastEventAt: now.Add(-90 * time.Second),
		Waiting: &model.Wait{
			Kind:    model.WaitQuestion,
			Prompt:  "How would you like to set the wallpaper path?",
			Options: []string{"Reference Downloads directly", "Copy into ~/.nixos"},
			Tool:    "question",
			Since:   now.Add(-90 * time.Second),
		},
	}
	m := newTestModel()
	m.width, m.height = 130, 40
	m.applySnapshot(registry.Snapshot{Sessions: []model.Session{waiting}})

	out := deANSI(m.View())
	for _, want := range []string{
		"1 waiting",                 // title bar count
		"WAITING FOR YOUR ANSWER",   // detail callout
		"How would you like to set", // the question
		"Reference Downloads",       // an option
	} {
		if !strings.Contains(out, want) {
			t.Errorf("waiting view missing %q\n---\n%s", want, out)
		}
	}

	// Waiting must sort to the top.
	if s, _ := m.selectedSession(); s.ID != "ses_wait" {
		t.Fatalf("waiting session not first/selected, got %s", s.ID)
	}
}
