package copilot

import (
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
		`"data":{"permissionRequest":{"kind":"read","path":"/x"}}}`)
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
	s.handleLine("s1", []byte(`{"type":"permission.requested","data":{"permissionRequest":{"kind":"shell"}}}`), out)
	_ = drain(out)
	s.handleLine("s1", []byte(`{"type":"permission.completed","data":{}}`), out)
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
