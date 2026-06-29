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

	mu    sync.Mutex
	tails map[string]*tail // sessionID -> tail state
}

type tail struct {
	path   string
	offset int64
	// perms holds the outstanding permission requests for this session, keyed
	// by requestId. Copilot emits an explicit `permission.requested` when the
	// agent is blocked on the user and a matching `permission.completed` when
	// answered, so a non-empty map is the authoritative "waiting" signal. This
	// correctly ignores slow-but-auto-approved tools (which never request a
	// permission) and supports multiple concurrent requests.
	perms map[string]*permRequest
	// waiting mirrors len(perms) > 0 at the last emit, so we only emit
	// wait-begin/end transitions (and refresh the prompt) when it changes.
	waiting bool
	ended   bool
	// titled is set once we've resolved a human-readable title from
	// workspace.yaml. Until then, scans keep retrying, because the file may not
	// exist (or may be unnamed) at the moment the session first appears.
	titled bool
}

// permRequest is one outstanding permission prompt awaiting the user.
type permRequest struct {
	wait model.Wait
	at   time.Time // ordering, so the newest request drives the shown prompt
}

// New constructs a Copilot source.
func New(stateDir string, pollInterval, recentWindow time.Duration) *Source {
	return &Source{
		stateDir:     stateDir,
		pollInterval: pollInterval,
		recentWindow: recentWindow,
		tails:        make(map[string]*tail),
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
		t, known := s.tails[id]
		s.mu.Unlock()
		if known {
			// Already tailing; tail reads handle updates. Retry workspace
			// metadata until we get a title — workspace.yaml may have been
			// absent or unnamed when the session first appeared.
			if t != nil && !t.titled {
				if meta := s.loadWorkspace(id); meta != nil && meta.Title != "" {
					emit(out, model.Event{Kind: model.EventUpsert, Source: model.SourceCopilot, ID: id, Session: meta, At: meta.UpdatedAt})
					s.mu.Lock()
					t.titled = true
					s.mu.Unlock()
				}
			}
			s.readTail(id, evPath, out)
			continue
		}
		if st.ModTime().Before(cutoff) {
			continue // too old to care about
		}

		// New, recent session. Emit workspace metadata first.
		titled := false
		if meta := s.loadWorkspace(id); meta != nil {
			emit(out, model.Event{Kind: model.EventUpsert, Source: model.SourceCopilot, ID: id, Session: meta, At: meta.UpdatedAt})
			titled = meta.Title != ""
		}
		s.mu.Lock()
		s.tails[id] = &tail{path: evPath, offset: 0, titled: titled}
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
		// Resuming clears any prior terminal state so the session reappears as
		// live, and drops stale in-flight tracking from before the shutdown.
		s.mu.Lock()
		var wasWaiting bool
		if t := s.tails[id]; t != nil {
			t.ended = false
			t.perms = nil
			wasWaiting = t.waiting
			t.waiting = false
		}
		s.mu.Unlock()
		if wasWaiting {
			emit(out, model.Event{Kind: model.EventWaitEnd, Source: model.SourceCopilot, ID: id, At: at})
		}
		emit(out, model.Event{Kind: model.EventResume, Source: model.SourceCopilot, ID: id, At: at})
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
		// A tool starting is just activity. Whether it blocks on the user is
		// signalled separately and authoritatively by `permission.requested`;
		// we deliberately do NOT infer waiting from execution duration, because
		// a slow but auto-approved tool is byte-identical to a blocked one
		// until a permission event arrives.
		emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceCopilot, ID: id, At: at,
			Item: &model.Activity{Kind: model.ActivityTool, Tool: d.ToolName, Text: summary, At: at}})

	case "tool.execution_complete":
		// Nothing to do for waiting state: permission.completed (not tool
		// completion) is what resolves an approval prompt. Emitting no activity
		// here avoids duplicate timeline entries for every tool.

	case "permission.requested":
		// Explicit, authoritative signal that the agent is blocked on the user
		// (directory/file read, shell command, write, url, mcp, …). Keyed by
		// requestId so concurrent prompts are tracked independently.
		reqID := permRequestID(ev.Data)
		w := parsePermission(ev.Data, at)
		s.recordPermission(id, reqID, &permRequest{wait: *w, at: at}, out)

	case "permission.completed":
		// The user answered (approved/denied/rejected) => this request resolves.
		reqID := permRequestID(ev.Data)
		s.resolvePermission(id, reqID, out)

	case "assistant.turn_end":
		// A turn cannot end while a permission is pending, so a turn boundary
		// is a safe point to clear any lingering wait (defensive).
		s.clearPermissions(id, at, out)

	case "abort", "session.error":
		s.clearPermissions(id, at, out)
		emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceCopilot, ID: id, At: at,
			Item: &model.Activity{Kind: model.ActivityError, Text: ev.Type, At: at}})

	case "session.shutdown":
		s.clearPermissions(id, at, out)
		s.mu.Lock()
		if t := s.tails[id]; t != nil {
			t.ended = true
		}
		s.mu.Unlock()
		emit(out, model.Event{Kind: model.EventEnded, Source: model.SourceCopilot, ID: id, At: at})
	}
}

// recordPermission registers an outstanding permission request and emits a
// wait-begin carrying the newest prompt. Idempotent on requestId so replays
// don't double-count.
func (s *Source) recordPermission(id, reqID string, pr *permRequest, out chan<- model.Event) {
	s.mu.Lock()
	t := s.tails[id]
	if t == nil {
		s.mu.Unlock()
		return
	}
	if t.perms == nil {
		t.perms = make(map[string]*permRequest)
	}
	if _, dup := t.perms[reqID]; dup {
		s.mu.Unlock()
		return
	}
	t.perms[reqID] = pr
	t.waiting = true
	w := currentWaitLocked(t)
	s.mu.Unlock()
	emit(out, model.Event{Kind: model.EventWaitBegin, Source: model.SourceCopilot, ID: id, At: pr.at, Wait: w})
}

// resolvePermission clears one answered request. If others remain, the wait
// continues with the next-newest prompt; otherwise the wait ends.
func (s *Source) resolvePermission(id, reqID string, out chan<- model.Event) {
	s.mu.Lock()
	t := s.tails[id]
	if t == nil || len(t.perms) == 0 {
		s.mu.Unlock()
		return
	}
	if reqID != "" {
		delete(t.perms, reqID)
	} else {
		// No correlation id: clear the oldest outstanding request.
		var oldestKey string
		var oldestAt time.Time
		for k, p := range t.perms {
			if oldestKey == "" || p.at.Before(oldestAt) {
				oldestKey, oldestAt = k, p.at
			}
		}
		delete(t.perms, oldestKey)
	}
	if len(t.perms) > 0 {
		w := currentWaitLocked(t)
		s.mu.Unlock()
		emit(out, model.Event{Kind: model.EventWaitBegin, Source: model.SourceCopilot, ID: id, At: time.Now(), Wait: w})
		return
	}
	t.waiting = false
	s.mu.Unlock()
	emit(out, model.Event{Kind: model.EventWaitEnd, Source: model.SourceCopilot, ID: id, At: time.Now()})
}

// clearPermissions drops all outstanding requests and ends any active wait.
func (s *Source) clearPermissions(id string, at time.Time, out chan<- model.Event) {
	s.mu.Lock()
	var clearWait bool
	if t := s.tails[id]; t != nil {
		t.perms = nil
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

// currentWaitLocked returns the wait describing the newest outstanding request.
// Caller must hold s.mu.
func currentWaitLocked(t *tail) *model.Wait {
	var newest *permRequest
	for _, p := range t.perms {
		if newest == nil || p.at.After(newest.at) {
			newest = p
		}
	}
	if newest == nil {
		return nil
	}
	w := newest.wait
	return &w
}

// workspaceYAML mirrors copilot's workspace.yaml.
type workspaceYAML struct {
	ID        string `yaml:"id"`
	Cwd       string `yaml:"cwd"`
	GitRoot   string `yaml:"git_root"`
	Branch    string `yaml:"branch"`
	Name      string `yaml:"name"`
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
	// Prefer the human-given session name; fall back to a generated summary,
	// then the working directory's name so the list never shows a bare UUID.
	title := w.Name
	if title == "" {
		title = w.Summary
	}
	if title == "" && w.Cwd != "" {
		title = filepath.Base(w.Cwd)
	}
	return &model.Session{
		ID:        id,
		Source:    model.SourceCopilot,
		Title:     firstLine(title, 120),
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

// permRequestID extracts the correlation id shared by permission.requested and
// permission.completed. Copilot puts `requestId` on both; we fall back to the
// toolCallId (also present on both) if requestId is missing.
func permRequestID(raw json.RawMessage) string {
	var d struct {
		RequestID         string `json:"requestId"`
		ToolCallID        string `json:"toolCallId"`
		PermissionRequest struct {
			ToolCallID string `json:"toolCallId"`
		} `json:"permissionRequest"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return ""
	}
	if d.RequestID != "" {
		return d.RequestID
	}
	if d.ToolCallID != "" {
		return d.ToolCallID
	}
	return d.PermissionRequest.ToolCallID
}

// parsePermission extracts a human-readable prompt and tool label from a
// copilot permission.requested event. The request kind (shell/read/write/url/
// mcp/memory) determines the most relevant detail field.
func parsePermission(raw json.RawMessage, at time.Time) *model.Wait {
	w := &model.Wait{Kind: model.WaitApproval, Since: at, Prompt: "needs your approval"}
	var d struct {
		PermissionRequest struct {
			Kind            string `json:"kind"`
			Intention       string `json:"intention"`
			FullCommandText string `json:"fullCommandText"`
			Path            string `json:"path"`
			FileName        string `json:"fileName"`
			URL             string `json:"url"`
			ToolName        string `json:"toolName"`
		} `json:"permissionRequest"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return w
	}
	pr := d.PermissionRequest
	w.Tool = pr.Kind
	// Prefer the agent's stated intention; fall back to the kind-specific
	// detail (command, path, url, …) when no intention is present.
	detail := pr.Intention
	if detail == "" {
		switch {
		case pr.FullCommandText != "":
			detail = pr.FullCommandText
		case pr.Path != "":
			detail = pr.Path
		case pr.FileName != "":
			detail = pr.FileName
		case pr.URL != "":
			detail = pr.URL
		case pr.ToolName != "":
			detail = pr.ToolName
		}
	}
	prompt := "approve " + pr.Kind
	if detail != "" {
		prompt += ": " + detail
	}
	w.Prompt = firstLine(prompt, 240)
	return w
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
