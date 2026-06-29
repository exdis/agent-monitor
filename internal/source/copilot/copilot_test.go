package copilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/exdis/agent-monitor/internal/model"
)

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
	s := New("", 100*time.Millisecond, 5*time.Minute)
	s.tails["s1"] = &tail{path: "events.jsonl"}
	return s
}

func TestLoadWorkspaceTitle(t *testing.T) {
	dir := t.TempDir()
	sid := "sess-1"
	if err := os.MkdirAll(filepath.Join(dir, sid), 0o755); err != nil {
		t.Fatal(err)
	}
	yml := "id: sess-1\ncwd: /home/u/dev/proj\nname: My Feature Work\nsummary: ignored\n"
	if err := os.WriteFile(filepath.Join(dir, sid, "workspace.yaml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(dir, time.Second, time.Hour)
	got := s.loadWorkspace(sid)
	if got == nil || got.Title != "My Feature Work" {
		t.Fatalf("title = %q, want name", got.Title)
	}

	// No name/summary => fall back to cwd basename, not the UUID.
	yml2 := "id: sess-1\ncwd: /home/u/dev/proj\n"
	_ = os.WriteFile(filepath.Join(dir, sid, "workspace.yaml"), []byte(yml2), 0o644)
	if got := s.loadWorkspace(sid); got.Title != "proj" {
		t.Fatalf("title = %q, want proj", got.Title)
	}
}

// TestScanRetriesTitleUntilWorkspaceAppears verifies that a session first seen
// without a workspace.yaml (so it would show a bare UUID) gets its title filled
// in on a later scan once the file appears.
func TestScanRetriesTitleUntilWorkspaceAppears(t *testing.T) {
	dir := t.TempDir()
	sid := "11111111-2222-3333-4444-555555555555"
	sdir := filepath.Join(dir, sid)
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// events.jsonl exists but workspace.yaml does not yet.
	if err := os.WriteFile(filepath.Join(sdir, "events.jsonl"),
		[]byte(`{"type":"user.message","data":{"content":"hi"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(dir, time.Second, time.Hour)
	out := make(chan model.Event, 64)
	s.scan(context.Background(), out, true)

	if s.tails[sid].titled {
		t.Fatal("should not be titled before workspace.yaml exists")
	}

	// workspace.yaml now appears with a name.
	if err := os.WriteFile(filepath.Join(sdir, "workspace.yaml"),
		[]byte("id: "+sid+"\ncwd: /home/u/dev/proj\nname: My Session\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.scan(context.Background(), out, false)

	if !s.tails[sid].titled {
		t.Fatal("expected titled after workspace.yaml appeared")
	}
	var gotTitle string
	for _, e := range drain(out) {
		if e.Session != nil && e.Session.Title != "" {
			gotTitle = e.Session.Title
		}
	}
	if gotTitle != "My Session" {
		t.Fatalf("title = %q, want My Session", gotTitle)
	}
}

func TestParsePermissionShell(t *testing.T) {
	data := json.RawMessage(`{"permissionRequest":{"kind":"shell","fullCommandText":"rm -rf /tmp/x","intention":"clean up"}}`)
	w := parsePermission(data, time.Now())
	if w.Kind != model.WaitApproval {
		t.Fatalf("kind = %q", w.Kind)
	}
	if w.Tool != "shell" {
		t.Errorf("tool = %q, want shell", w.Tool)
	}
	if w.Prompt != "approve shell: clean up" {
		t.Errorf("prompt = %q", w.Prompt)
	}
}

func TestParsePermissionReadFallsBackToPath(t *testing.T) {
	data := json.RawMessage(`{"permissionRequest":{"kind":"read","path":"/home/u/secret.txt"}}`)
	w := parsePermission(data, time.Now())
	if w.Tool != "read" || w.Prompt != "approve read: /home/u/secret.txt" {
		t.Fatalf("unexpected wait: tool=%q prompt=%q", w.Tool, w.Prompt)
	}
}

func TestPermissionRequestedEmitsWaitImmediately(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 16)
	line := []byte(`{"type":"permission.requested","timestamp":"2026-06-29T10:00:00Z",` +
		`"data":{"requestId":"r1","permissionRequest":{"kind":"read","toolCallId":"tc1","path":"/x"}}}`)
	s.handleLine("s1", line, out)

	var begin *model.Event
	for _, e := range drain(out) {
		if e.Kind == model.EventWaitBegin {
			ev := e
			begin = &ev
		}
	}
	if begin == nil || begin.Wait == nil || begin.Wait.Kind != model.WaitApproval {
		t.Fatalf("expected immediate WaitBegin, got %+v", begin)
	}
	if !s.tails["s1"].waiting {
		t.Error("session not marked waiting")
	}
}

func TestPermissionCompletedClearsWait(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 16)
	s.handleLine("s1", []byte(`{"type":"permission.requested","data":{"requestId":"r1","permissionRequest":{"kind":"shell","toolCallId":"tc1"}}}`), out)
	_ = drain(out)
	s.handleLine("s1", []byte(`{"type":"permission.completed","data":{"requestId":"r1","toolCallId":"tc1","result":{"kind":"approved"}}}`), out)
	var end bool
	for _, e := range drain(out) {
		if e.Kind == model.EventWaitEnd {
			end = true
		}
	}
	if !end || s.tails["s1"].waiting {
		t.Fatalf("expected wait cleared on completion")
	}
}

func TestResumeAfterShutdownRevives(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 16)
	s.handleLine("s1", []byte(`{"type":"session.shutdown","data":{}}`), out)
	if !s.tails["s1"].ended {
		t.Fatal("expected ended after shutdown")
	}
	_ = drain(out)
	s.handleLine("s1", []byte(`{"type":"session.resume","data":{"context":{"cwd":"/p"}}}`), out)
	if s.tails["s1"].ended {
		t.Fatal("resume should clear ended")
	}
	var resumed bool
	for _, e := range drain(out) {
		if e.Kind == model.EventResume {
			resumed = true
		}
	}
	if !resumed {
		t.Fatal("expected EventResume on resume")
	}
}

// TestSlowAutoApprovedToolNeverWaits is the core regression: a tool that runs
// for a long time but is auto-approved (no permission.requested) must NEVER be
// reported as waiting, no matter how long it takes.
func TestSlowAutoApprovedToolNeverWaits(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 32)
	// Tool starts, runs "forever", then completes — with no permission event.
	s.handleLine("s1", []byte(`{"type":"tool.execution_start","data":{"toolCallId":"tc1","toolName":"bash","arguments":{"command":"sleep 999"}}}`), out)
	s.handleLine("s1", []byte(`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","success":true}}`), out)
	for _, e := range drain(out) {
		if e.Kind == model.EventWaitBegin {
			t.Fatalf("auto-approved tool wrongly reported waiting: %+v", e.Wait)
		}
	}
	if s.tails["s1"].waiting {
		t.Fatal("session should not be waiting for an auto-approved tool")
	}
}

// TestConcurrentPermissionsTrackedIndependently verifies two outstanding
// requests keep the session waiting until both resolve, and the shown prompt
// follows the newest request.
func TestConcurrentPermissionsTrackedIndependently(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 64)
	s.handleLine("s1", []byte(`{"type":"permission.requested","timestamp":"2026-06-29T10:00:00Z","data":{"requestId":"r1","permissionRequest":{"kind":"shell","intention":"first"}}}`), out)
	s.handleLine("s1", []byte(`{"type":"permission.requested","timestamp":"2026-06-29T10:00:05Z","data":{"requestId":"r2","permissionRequest":{"kind":"write","intention":"second"}}}`), out)
	_ = drain(out)
	if len(s.tails["s1"].perms) != 2 {
		t.Fatalf("expected 2 outstanding perms, got %d", len(s.tails["s1"].perms))
	}

	// Resolve the newer one; still waiting on the first, no WaitEnd yet.
	s.handleLine("s1", []byte(`{"type":"permission.completed","data":{"requestId":"r2","result":{"kind":"approved"}}}`), out)
	for _, e := range drain(out) {
		if e.Kind == model.EventWaitEnd {
			t.Fatal("must not end wait while a request is still outstanding")
		}
	}
	if !s.tails["s1"].waiting {
		t.Fatal("still expected waiting with one outstanding request")
	}

	// Resolve the last one => wait ends.
	s.handleLine("s1", []byte(`{"type":"permission.completed","data":{"requestId":"r1","result":{"kind":"approved"}}}`), out)
	var ended bool
	for _, e := range drain(out) {
		if e.Kind == model.EventWaitEnd {
			ended = true
		}
	}
	if !ended || s.tails["s1"].waiting {
		t.Fatal("expected wait to end once all requests resolved")
	}
}

// TestPermissionRequestedIdempotent ensures a replayed requested event (same
// requestId) doesn't double-count.
func TestPermissionRequestedIdempotent(t *testing.T) {
	s := newTestSource()
	out := make(chan model.Event, 16)
	line := []byte(`{"type":"permission.requested","data":{"requestId":"r1","permissionRequest":{"kind":"shell"}}}`)
	s.handleLine("s1", line, out)
	s.handleLine("s1", line, out)
	if n := len(s.tails["s1"].perms); n != 1 {
		t.Fatalf("expected 1 perm after duplicate, got %d", n)
	}
}

