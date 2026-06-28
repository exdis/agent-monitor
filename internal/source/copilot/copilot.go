// Package copilot implements a passive, read-only Source for the GitHub Copilot
// CLI. Copilot persists each session under
// ~/.copilot/session-state/<uuid>/ with an append-only events.jsonl and a
// workspace.yaml describing the working directory and git context.
//
// We watch the session-state directory for new/updated sessions and tail each
// events.jsonl from the last byte offset, normalizing Copilot's native events
// into the shared model.
package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"

	"github.com/exdis/agent-monitor/internal/model"
)

// Source tails copilot per-session event logs.
type Source struct {
	stateDir     string
	pollInterval time.Duration
	recentWindow time.Duration
	// approvalGrace is how long a tool may run before a still-incomplete
	// execution is treated as "waiting for approval". Copilot's permission
	// prompts block the tool indefinitely, while auto-approved tools complete
	// quickly, so a short grace period separates the two.
	approvalGrace time.Duration

	mu    sync.Mutex
	tails map[string]*tail // sessionID -> tail state
}

type tail struct {
	path   string
	offset int64
	// pending tracks tool executions that started but haven't completed,
	// keyed by toolCallId. Used to detect "waiting for approval".
	pending map[string]*pendingTool
	// waiting is true once we've emitted a wait-begin for this session.
	waiting bool
	ended   bool
}

type pendingTool struct {
	tool    string
	summary string
	started time.Time
}

// New constructs a Copilot source.
func New(stateDir string, pollInterval, recentWindow time.Duration) *Source {
	return &Source{
		stateDir:      stateDir,
		pollInterval:  pollInterval,
		recentWindow:  recentWindow,
		approvalGrace: 4 * time.Second,
		tails:         make(map[string]*tail),
	}
}

// Kind identifies this source.
func (s *Source) Kind() model.SourceKind { return model.SourceCopilot }

// Run discovers sessions, watches for changes, and tails event logs until ctx
// is cancelled.
func (s *Source) Run(ctx context.Context, out chan<- model.Event) error {
	// Initial scan of recently-updated sessions.
	s.scan(ctx, out, true)

	// Try fsnotify for low-latency updates; fall back to polling if it fails.
	watcher, werr := fsnotify.NewWatcher()
	if werr == nil {
		defer watcher.Close()
		_ = watcher.Add(s.stateDir) // watch dir for new session subdirs
		s.mu.Lock()
		for _, t := range s.tails {
			_ = watcher.Add(filepath.Dir(t.path))
		}
		s.mu.Unlock()
	}

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Periodic full scan catches new sessions and is the fallback when
			// fsnotify is unavailable.
			s.scan(ctx, out, false)
			// Promote long-dangling tool executions to "waiting for approval".
			s.checkApproval(out)
			if watcher != nil {
				s.rewatch(watcher)
			}
		case ev, ok := <-watcherEvents(watcher):
			if !ok {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				if strings.HasSuffix(ev.Name, "events.jsonl") {
					id := filepath.Base(filepath.Dir(ev.Name))
					s.readTail(id, ev.Name, out)
				} else {
					// A new session directory likely appeared.
					s.scan(ctx, out, false)
					s.rewatch(watcher)
				}
			}
		}
	}
}

// watcherEvents returns the watcher's event channel, or a nil channel (blocks
// forever) when fsnotify is unavailable.
func watcherEvents(w *fsnotify.Watcher) <-chan fsnotify.Event {
	if w == nil {
		return nil
	}
	return w.Events
}

// rewatch ensures each known session directory is being watched.
func (s *Source) rewatch(w *fsnotify.Watcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tails {
		_ = w.Add(filepath.Dir(t.path))
	}
}

// scan enumerates session directories, registering tails for ones updated
// within the recent window. When seed is true (first scan) we backfill metadata
// and a small slice of recent activity.
func (s *Source) scan(ctx context.Context, out chan<- model.Event, seed bool) {
	entries, err := os.ReadDir(s.stateDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-s.recentWindow)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		evPath := filepath.Join(s.stateDir, id, "events.jsonl")
		st, err := os.Stat(evPath)
		if err != nil {
			continue
		}
		s.mu.Lock()
		_, known := s.tails[id]
		s.mu.Unlock()
		if known {
			// Already tailing; tail reads handle updates.
			s.readTail(id, evPath, out)
			continue
		}
		if st.ModTime().Before(cutoff) {
			continue // too old to care about
		}

		// New, recent session. Emit workspace metadata first.
		if meta := s.loadWorkspace(id); meta != nil {
			emit(out, model.Event{Kind: model.EventUpsert, Source: model.SourceCopilot, ID: id, Session: meta, At: meta.UpdatedAt})
		}
		s.mu.Lock()
		s.tails[id] = &tail{path: evPath, offset: 0}
		s.mu.Unlock()
		// Read from the beginning so we capture the current action; the
		// recent-window filter already bounds how much history this is.
		s.readTail(id, evPath, out)
	}
}

// readTail reads new lines appended since the last offset and emits events.
func (s *Source) readTail(id, path string, out chan<- model.Event) {
	s.mu.Lock()
	t := s.tails[id]
	if t == nil {
		t = &tail{path: path}
		s.tails[id] = t
	}
	off := t.offset
	s.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	if off > 0 {
		if _, err := f.Seek(off, 0); err != nil {
			off = 0
			_, _ = f.Seek(0, 0)
		}
	}
	r := bufio.NewReaderSize(f, 64*1024)
	var consumed int64 = off
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		consumed += int64(len(line)) + 1 // +1 for newline
		s.handleLine(id, line, out)
	}
	s.mu.Lock()
	if tt := s.tails[id]; tt != nil {
		tt.offset = consumed
	}
	s.mu.Unlock()
}

// copilotEvent is the envelope of a single events.jsonl line.
type copilotEvent struct {
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	Timestamp string          `json:"timestamp"`
}

func (s *Source) handleLine(id string, line []byte, out chan<- model.Event) {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return
	}
	var ev copilotEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	at := parseTime(ev.Timestamp)
	if at.IsZero() {
		at = time.Now()
	}

	switch ev.Type {
	case "session.start", "session.resume":
		var d struct {
			Context struct {
				Cwd     string `json:"cwd"`
				GitRoot string `json:"gitRoot"`
				Branch  string `json:"branch"`
			} `json:"context"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		sess := &model.Session{ID: id, Source: model.SourceCopilot, Directory: d.Context.Cwd, Branch: d.Context.Branch}
		emit(out, model.Event{Kind: model.EventUpsert, Source: model.SourceCopilot, ID: id, Session: sess, At: at})

	case "session.model_change":
		var d struct {
			NewModel string `json:"newModel"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		emit(out, model.Event{Kind: model.EventUpsert, Source: model.SourceCopilot, ID: id,
			Session: &model.Session{ID: id, Source: model.SourceCopilot, Model: d.NewModel}, At: at})

	case "user.message":
		var d struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceCopilot, ID: id, At: at,
			Item: &model.Activity{Kind: model.ActivityMessage, Role: model.RoleUser, Text: firstLine(d.Content, 200), At: at}})

	case "assistant.turn_start":
		emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceCopilot, ID: id, At: at,
			Item: &model.Activity{Kind: model.ActivityTurn, Role: model.RoleAssistant, Text: "thinking", At: at}})

	case "assistant.message":
		var d struct {
			Content       string `json:"content"`
			ReasoningText string `json:"reasoningText"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		text := strings.TrimSpace(d.Content)
		if text == "" {
			text = strings.TrimSpace(d.ReasoningText)
		}
		if text != "" {
			emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceCopilot, ID: id, At: at,
				Item: &model.Activity{Kind: model.ActivityMessage, Role: model.RoleAssistant, Text: firstLine(text, 200), At: at}})
		}

	case "tool.execution_start":
		var d struct {
			ToolCallID string          `json:"toolCallId"`
			ToolName   string          `json:"toolName"`
			Arguments  json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		summary := summarizeInput(d.Arguments)
		// Track this execution as in-flight; if it doesn't complete within the
		// approval grace period we treat the session as waiting for approval.
		s.mu.Lock()
		if t := s.tails[id]; t != nil {
			if t.pending == nil {
				t.pending = make(map[string]*pendingTool)
			}
			t.pending[d.ToolCallID] = &pendingTool{tool: d.ToolName, summary: summary, started: at}
		}
		s.mu.Unlock()
		emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceCopilot, ID: id, At: at,
			Item: &model.Activity{Kind: model.ActivityTool, Tool: d.ToolName, Text: summary, At: at}})

	case "tool.execution_complete":
		var d struct {
			ToolCallID string `json:"toolCallId"`
			Success    bool   `json:"success"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		// Execution finished => no longer waiting on it. If it was the one that
		// put us into a waiting state, clear waiting.
		s.mu.Lock()
		var clearWait bool
		if t := s.tails[id]; t != nil {
			delete(t.pending, d.ToolCallID)
			if t.waiting && len(t.pending) == 0 {
				t.waiting = false
				clearWait = true
			}
		}
		s.mu.Unlock()
		if clearWait {
			emit(out, model.Event{Kind: model.EventWaitEnd, Source: model.SourceCopilot, ID: id, At: at})
		}

	case "assistant.turn_end":
		// Turn ended; any in-flight tool tracking is no longer meaningful.
		s.clearPending(id, at, out)

	case "abort", "session.error":
		s.clearPending(id, at, out)
		emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceCopilot, ID: id, At: at,
			Item: &model.Activity{Kind: model.ActivityError, Text: ev.Type, At: at}})

	case "session.shutdown":
		s.clearPending(id, at, out)
		s.mu.Lock()
		if t := s.tails[id]; t != nil {
			t.ended = true
		}
		s.mu.Unlock()
		emit(out, model.Event{Kind: model.EventEnded, Source: model.SourceCopilot, ID: id, At: at})
	}
}

// clearPending drops all in-flight tool tracking for a session and clears any
// active waiting state.
func (s *Source) clearPending(id string, at time.Time, out chan<- model.Event) {
	s.mu.Lock()
	var clearWait bool
	if t := s.tails[id]; t != nil {
		t.pending = nil
		if t.waiting {
			t.waiting = false
			clearWait = true
		}
	}
	s.mu.Unlock()
	if clearWait {
		emit(out, model.Event{Kind: model.EventWaitEnd, Source: model.SourceCopilot, ID: id, At: at})
	}
}

// checkApproval promotes any tool that has been in-flight longer than the
// approval grace period into a "waiting for approval" state. Called each poll.
func (s *Source) checkApproval(out chan<- model.Event) {
	now := time.Now()
	type beginEv struct {
		id   string
		wait model.Wait
	}
	var begins []beginEv
	s.mu.Lock()
	for id, t := range s.tails {
		if t.waiting || t.ended || len(t.pending) == 0 {
			continue
		}
		// Find the oldest in-flight tool past the grace period.
		var oldest *pendingTool
		for _, pt := range t.pending {
			elapsed := now.Sub(pt.started)
			if elapsed < s.approvalGrace {
				continue
			}
			// Ignore ancient dangling starts (e.g. replayed from an old log on
			// first scan, or a long-dead session): a real approval prompt is
			// recent. recentWindow bounds how fresh it must be.
			if elapsed > s.recentWindow {
				continue
			}
			if oldest == nil || pt.started.Before(oldest.started) {
				oldest = pt
			}
		}
		if oldest != nil {
			t.waiting = true
			prompt := "approve " + oldest.tool
			if oldest.summary != "" {
				prompt += ": " + oldest.summary
			}
			begins = append(begins, beginEv{id: id, wait: model.Wait{
				Kind: model.WaitApproval, Prompt: firstLine(prompt, 240), Tool: oldest.tool, Since: oldest.started,
			}})
		}
	}
	s.mu.Unlock()
	for _, b := range begins {
		w := b.wait
		emit(out, model.Event{Kind: model.EventWaitBegin, Source: model.SourceCopilot, ID: b.id, At: time.Now(), Wait: &w})
	}
}

// workspaceYAML mirrors copilot's workspace.yaml.
type workspaceYAML struct {
	ID        string `yaml:"id"`
	Cwd       string `yaml:"cwd"`
	GitRoot   string `yaml:"git_root"`
	Branch    string `yaml:"branch"`
	Summary   string `yaml:"summary"`
	CreatedAt string `yaml:"created_at"`
	UpdatedAt string `yaml:"updated_at"`
}

func (s *Source) loadWorkspace(id string) *model.Session {
	path := filepath.Join(s.stateDir, id, "workspace.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var w workspaceYAML
	if err := yaml.Unmarshal(b, &w); err != nil {
		return nil
	}
	return &model.Session{
		ID:        id,
		Source:    model.SourceCopilot,
		Title:     firstLine(w.Summary, 120),
		Directory: w.Cwd,
		Branch:    w.Branch,
		CreatedAt: parseTime(w.CreatedAt),
		UpdatedAt: parseTime(w.UpdatedAt),
	}
}

// --- helpers ---

func emit(out chan<- model.Event, ev model.Event) {
	defer func() { _ = recover() }()
	out <- ev
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

func summarizeInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range []string{"command", "filePath", "file_path", "path", "pattern", "query", "url", "description", "prompt"} {
		if v, ok := m[k]; ok {
			if str, ok := v.(string); ok && str != "" {
				return firstLine(str, 120)
			}
		}
	}
	return ""
}
