package opencode

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/exdis/agent-monitor/internal/model"
)

func TestIsQuestionTool(t *testing.T) {
	cases := map[string]bool{
		"question":              true,
		"ask":                   true,
		"ask_followup_question": true,
		"bash":                  false,
		"read":                  false,
		"":                      false,
	}
	for in, want := range cases {
		if got := isQuestionTool(in); got != want {
			t.Errorf("isQuestionTool(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseQuestionWait(t *testing.T) {
	input := json.RawMessage(`{
		"questions": [
			{
				"question": "How would you like to set the wallpaper path?",
				"header": "Wallpaper source",
				"options": [
					{"label": "Reference Downloads path directly", "description": "simplest"},
					{"label": "Copy image into ~/.nixos", "description": "portable"}
				]
			}
		]
	}`)
	w := parseQuestionWait(input, time.Now())
	if w == nil {
		t.Fatal("expected a wait, got nil")
	}
	if w.Prompt != "How would you like to set the wallpaper path?" {
		t.Errorf("prompt = %q", w.Prompt)
	}
	if len(w.Options) != 2 || w.Options[0] != "Reference Downloads path directly" {
		t.Errorf("options = %v", w.Options)
	}
	if w.Tool != "question" {
		t.Errorf("tool = %q", w.Tool)
	}
}

func TestParseQuestionWaitMultiple(t *testing.T) {
	input := json.RawMessage(`{"questions":[{"question":"Q1","options":[]},{"question":"Q2","options":[]}]}`)
	w := parseQuestionWait(input, time.Now())
	if w == nil {
		t.Fatal("nil wait")
	}
	// Should note that more questions follow.
	if want := "Q1 (+1 more)"; w.Prompt != want {
		t.Errorf("prompt = %q, want %q", w.Prompt, want)
	}
}

func TestParseQuestionWaitEmpty(t *testing.T) {
	w := parseQuestionWait(nil, time.Now())
	if w == nil || w.Prompt == "" {
		t.Fatalf("expected fallback wait with prompt, got %+v", w)
	}
}

func TestParseModel(t *testing.T) {
	cases := map[string]string{
		`{"id":"claude-opus-4.8","providerID":"github-copilot"}`: "github-copilot/claude-opus-4.8",
		`{"id":"gpt-5"}`: "gpt-5",
		`plain-string`:   "plain-string",
		``:               "",
	}
	for in, want := range cases {
		if got := parseModel(in); got != want {
			t.Errorf("parseModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// drain collects all events currently buffered on the channel.
func drain(ch chan model.Event) []model.Event {
	var evs []model.Event
	for {
		select {
		case e := <-ch:
			evs = append(evs, e)
		default:
			return evs
		}
	}
}

func newTestSource() *Source {
	s := New("", 100*time.Millisecond, 5*time.Minute, 4*time.Second)
	return s
}

func TestPermissionWaitAfterGrace(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 16)

	// A bash tool that went pending 6s ago (past the 4s grace).
	s.trackPending("ses_1", "prt_a", "bash", "rm -rf /data", time.Now().Add(-6*time.Second))
	s.checkPermission(out)

	evs := drain(out)
	if len(evs) != 1 {
		t.Fatalf("expected 1 wait-begin, got %d", len(evs))
	}
	e := evs[0]
	if e.Kind != model.EventWaitBegin || e.Wait == nil {
		t.Fatalf("expected WaitBegin with Wait, got %+v", e)
	}
	if e.Wait.Kind != model.WaitPermission {
		t.Errorf("wait kind = %q, want permission", e.Wait.Kind)
	}
	if e.Wait.Tool != "bash" || e.Wait.Prompt != "approve bash: rm -rf /data" {
		t.Errorf("unexpected wait: tool=%q prompt=%q", e.Wait.Tool, e.Wait.Prompt)
	}

	// Calling again must not re-emit (already waiting).
	s.checkPermission(out)
	if got := len(drain(out)); got != 0 {
		t.Errorf("expected no duplicate wait-begin, got %d", got)
	}
}

func TestPermissionNoWaitWithinGrace(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 16)

	// Pending only 1s ago — within grace, should not trigger a wait yet.
	s.trackPending("ses_1", "prt_a", "bash", "ls", time.Now().Add(-1*time.Second))
	s.checkPermission(out)
	if got := len(drain(out)); got != 0 {
		t.Fatalf("expected no wait within grace, got %d", got)
	}
}

func TestPermissionWaitClearedOnResolve(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 16)

	s.trackPending("ses_1", "prt_a", "bash", "ls", time.Now().Add(-6*time.Second))
	s.checkPermission(out)
	_ = drain(out) // consume the wait-begin

	// Tool resolves (e.g. approved -> running): clearPending must end the wait.
	s.clearPending("ses_1", "prt_a", out)
	evs := drain(out)
	if len(evs) != 1 || evs[0].Kind != model.EventWaitEnd {
		t.Fatalf("expected one WaitEnd on resolve, got %+v", evs)
	}
	if s.waiting["ses_1"] {
		t.Error("session still marked waiting after resolve")
	}
}

func TestPermissionIgnoresAncientPending(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 16)

	// Pending far older than the recent window (e.g. replayed from an old log).
	s.trackPending("ses_1", "prt_a", "bash", "ls", time.Now().Add(-1*time.Hour))
	s.checkPermission(out)
	if got := len(drain(out)); got != 0 {
		t.Fatalf("expected ancient pending to be ignored, got %d", got)
	}
}

// TestPermissionRunningNoEnd exercises the real production path: opencode writes
// a permission-blocked tool as `running` with a start but no end time. handlePart
// must track it so checkPermission promotes it after the grace, and a later
// completed event must clear it.
func TestPermissionRunningNoEnd(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 64)
	start := time.Now().Add(-6 * time.Second).UnixMilli()

	// running bash, no end time => awaiting permission.
	running := `{"part":{"type":"tool","tool":"bash","callID":"toolu_1",` +
		`"state":{"status":"running","input":{"command":"echo hi"},"time":{"start":` +
		itoaJSON(start) + `}}},"time":` + itoaJSON(time.Now().UnixMilli()) + `}`
	s.handlePart("ses_1", running, out)
	_ = drain(out) // activity events
	s.checkPermission(out)

	var begin *model.Event
	for _, e := range drain(out) {
		if e.Kind == model.EventWaitBegin {
			ev := e
			begin = &ev
		}
	}
	if begin == nil || begin.Wait == nil || begin.Wait.Kind != model.WaitPermission {
		t.Fatalf("expected permission WaitBegin from running-no-end tool, got %+v", begin)
	}
	if begin.Wait.Tool != "bash" {
		t.Errorf("tool = %q, want bash", begin.Wait.Tool)
	}

	// Now the tool completes (approved & ran): the wait must clear.
	end := time.Now().UnixMilli()
	completed := `{"part":{"type":"tool","tool":"bash","callID":"toolu_1",` +
		`"state":{"status":"completed","input":{"command":"echo hi"},"time":{"start":` +
		itoaJSON(start) + `,"end":` + itoaJSON(end) + `}}},"time":` + itoaJSON(end) + `}`
	s.handlePart("ses_1", completed, out)
	var gotEnd bool
	for _, e := range drain(out) {
		if e.Kind == model.EventWaitEnd {
			gotEnd = true
		}
	}
	if !gotEnd {
		t.Error("expected WaitEnd after tool completed")
	}
	if s.waiting["ses_1"] {
		t.Error("session still waiting after completion")
	}
}

func itoaJSON(n int64) string { return strconv.FormatInt(n, 10) }
