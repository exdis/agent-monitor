package registry

import (
	"testing"
	"time"

	"github.com/exdis/agent-monitor/internal/config"
	"github.com/exdis/agent-monitor/internal/model"
)

func testConfig() config.Config {
	c := config.Default()
	c.ActiveThreshold = 30 * time.Second
	c.StaleThreshold = 5 * time.Minute
	c.RecentWindow = 15 * time.Minute
	c.MaxRecent = 10
	return c
}

func upsert(id string, src model.SourceKind, at time.Time) model.Event {
	return model.Event{
		Kind: model.EventUpsert, Source: src, ID: id, At: at,
		Session: &model.Session{ID: id, Source: src, Title: "t"},
	}
}

func TestLivenessStates(t *testing.T) {
	now := time.Now()

	// Fresh event => active.
	r1 := New(testConfig())
	r1.apply(upsert("s1", model.SourceOpenCode, now))
	if got := r1.Snapshot().Sessions[0].State; got != model.StateActive {
		t.Fatalf("fresh session state = %q, want active", got)
	}

	// 1 minute old => idle. (Heartbeat never moves backwards, so seed a fresh
	// registry whose only event is the old one.)
	r2 := New(testConfig())
	r2.apply(upsert("s1", model.SourceOpenCode, now.Add(-time.Minute)))
	r2.recompute()
	if got := find(r2, "opencode:s1").State; got != model.StateIdle {
		t.Fatalf("1m-old session = %q, want idle", got)
	}

	// 10 minutes old => stale.
	r3 := New(testConfig())
	r3.apply(upsert("s1", model.SourceOpenCode, now.Add(-10*time.Minute)))
	r3.recompute()
	if got := find(r3, "opencode:s1").State; got != model.StateStale {
		t.Fatalf("10m-old session = %q, want stale", got)
	}
}

func TestEndedIsTerminal(t *testing.T) {
	r := New(testConfig())
	now := time.Now()
	r.apply(upsert("s1", model.SourceCopilot, now))
	r.apply(model.Event{Kind: model.EventEnded, Source: model.SourceCopilot, ID: "s1", At: now})
	if got := find(r, "copilot:s1").State; got != model.StateEnded {
		t.Fatalf("state = %q, want ended", got)
	}
	// A later recompute must not resurrect it.
	r.apply(upsert("s1", model.SourceCopilot, now))
	r.recompute()
	if got := find(r, "copilot:s1").State; got != model.StateEnded {
		t.Fatalf("ended session changed to %q", got)
	}
}

func TestPruneOldSessions(t *testing.T) {
	c := testConfig()
	c.RecentWindow = time.Minute
	r := New(c)
	old := time.Now().Add(-2 * time.Minute)
	r.apply(upsert("s1", model.SourceOpenCode, old))
	r.recompute() // should prune (older than recent window, not active)
	if len(r.Snapshot().Sessions) != 0 {
		t.Fatalf("expected session pruned, got %d", len(r.Snapshot().Sessions))
	}
}

func TestActivityRingCapped(t *testing.T) {
	c := testConfig()
	c.MaxRecent = 3
	r := New(c)
	now := time.Now()
	for i := 0; i < 10; i++ {
		r.apply(model.Event{
			Kind: model.EventActivity, Source: model.SourceOpenCode, ID: "s1", At: now,
			Item: &model.Activity{Kind: model.ActivityTool, Tool: "bash", At: now},
		})
	}
	if got := len(find(r, "opencode:s1").Recent); got != 3 {
		t.Fatalf("recent length = %d, want 3 (capped)", got)
	}
}

func TestActionFromActivity(t *testing.T) {
	r := New(testConfig())
	now := time.Now()
	r.apply(model.Event{
		Kind: model.EventActivity, Source: model.SourceOpenCode, ID: "s1", At: now,
		Item: &model.Activity{Kind: model.ActivityTool, Tool: "bash", Text: "ls", At: now},
	})
	if got := find(r, "opencode:s1").CurrentAction; got != "running bash: ls" {
		t.Fatalf("current action = %q", got)
	}
}

func find(r *Registry, key string) model.Session {
	for _, s := range r.Snapshot().Sessions {
		if s.GlobalKey() == key {
			return s
		}
	}
	return model.Session{}
}

func TestWaitingTakesPriority(t *testing.T) {
	r := New(testConfig())
	now := time.Now()
	// Active session...
	r.apply(upsert("s1", model.SourceOpenCode, now))
	if find(r, "opencode:s1").State != model.StateActive {
		t.Fatal("expected active before wait")
	}
	// ...then a question begins => waiting overrides active.
	r.apply(model.Event{
		Kind: model.EventWaitBegin, Source: model.SourceOpenCode, ID: "s1", At: now,
		Wait: &model.Wait{Kind: model.WaitQuestion, Prompt: "pick one"},
	})
	s := find(r, "opencode:s1")
	if s.State != model.StateWaiting {
		t.Fatalf("state = %q, want waiting", s.State)
	}
	if s.Waiting == nil || s.Waiting.Prompt != "pick one" {
		t.Fatalf("waiting prompt not set: %+v", s.Waiting)
	}
}

func TestWaitingPersistsPastActiveThreshold(t *testing.T) {
	r := New(testConfig())
	old := time.Now().Add(-2 * time.Minute) // older than active threshold
	r.apply(model.Event{
		Kind: model.EventWaitBegin, Source: model.SourceOpenCode, ID: "s1", At: old,
		Wait: &model.Wait{Kind: model.WaitQuestion, Prompt: "still waiting", Since: old},
	})
	r.recompute()
	// A pending question must not decay to idle/stale just because time passed.
	if got := find(r, "opencode:s1").State; got != model.StateWaiting {
		t.Fatalf("state = %q, want waiting (should not decay)", got)
	}
}

func TestWaitEndResolvesToHeartbeat(t *testing.T) {
	r := New(testConfig())
	now := time.Now()
	r.apply(upsert("s1", model.SourceOpenCode, now))
	r.apply(model.Event{
		Kind: model.EventWaitBegin, Source: model.SourceOpenCode, ID: "s1", At: now,
		Wait: &model.Wait{Kind: model.WaitQuestion, Prompt: "q"},
	})
	if find(r, "opencode:s1").State != model.StateWaiting {
		t.Fatal("expected waiting")
	}
	// Answer the question => back to active (recent heartbeat).
	r.apply(model.Event{Kind: model.EventWaitEnd, Source: model.SourceOpenCode, ID: "s1", At: now})
	s := find(r, "opencode:s1")
	if s.State != model.StateActive {
		t.Fatalf("state = %q, want active after wait end", s.State)
	}
	if s.Waiting != nil {
		t.Fatalf("waiting not cleared: %+v", s.Waiting)
	}
}

func TestWaitingNotPruned(t *testing.T) {
	c := testConfig()
	c.RecentWindow = time.Minute
	r := New(c)
	old := time.Now().Add(-10 * time.Minute)
	r.apply(model.Event{
		Kind: model.EventWaitBegin, Source: model.SourceOpenCode, ID: "s1", At: old,
		Wait: &model.Wait{Kind: model.WaitQuestion, Prompt: "q", Since: old},
	})
	r.recompute() // must NOT prune a waiting session even though it's old
	if len(r.Snapshot().Sessions) != 1 {
		t.Fatalf("waiting session was pruned: %d", len(r.Snapshot().Sessions))
	}
}

func TestEndedClearsWaiting(t *testing.T) {
	r := New(testConfig())
	now := time.Now()
	r.apply(model.Event{
		Kind: model.EventWaitBegin, Source: model.SourceCopilot, ID: "s1", At: now,
		Wait: &model.Wait{Kind: model.WaitApproval, Prompt: "approve bash"},
	})
	r.apply(model.Event{Kind: model.EventEnded, Source: model.SourceCopilot, ID: "s1", At: now})
	s := find(r, "copilot:s1")
	if s.State != model.StateEnded || s.Waiting != nil {
		t.Fatalf("ended should clear waiting: state=%q waiting=%v", s.State, s.Waiting)
	}
}
