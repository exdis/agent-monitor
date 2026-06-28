// Package opencode implements a passive, read-only Source for OpenCode by
// tailing the append-only `event` table in opencode's live SQLite database.
//
// OpenCode persists every session event into a WAL-mode SQLite DB. Concurrent
// read-only access works while opencode itself is writing, so we can observe
// sessions started in any terminal without spawning a server.
package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/exdis/agent-monitor/internal/model"
)

// Source tails opencode's SQLite event table.
type Source struct {
	dbPath       string
	pollInterval time.Duration
	recentWindow time.Duration
	// permissionGrace is how long a tool may sit in `pending` before we treat it
	// as awaiting permission. opencode writes a tool part as `pending` and only
	// transitions it to `running` once the (SSE-only, non-persisted) permission
	// prompt is approved, so a tool stuck in `pending` ~= waiting on the user.
	permissionGrace time.Duration

	db     *sql.DB
	cursor int64 // last processed event rowid
	primed bool

	// pending tracks tool parts currently in `pending` status, keyed by
	// sessionID then partID, used to detect permission waits. A non-question
	// tool that stays pending past permissionGrace becomes a WaitPermission.
	pending map[string]map[string]*pendingTool
	// waiting marks sessions for which we've emitted a permission WaitBegin.
	waiting map[string]bool
}

type pendingTool struct {
	tool    string
	summary string
	since   time.Time
}

// New constructs an OpenCode source. permissionGrace controls how long a tool
// may stay unfinished before being treated as awaiting permission (<=0 uses the
// default).
func New(dbPath string, pollInterval, recentWindow, permissionGrace time.Duration) *Source {
	if permissionGrace <= 0 {
		permissionGrace = 20 * time.Second
	}
	return &Source{
		dbPath:       dbPath,
		pollInterval: pollInterval,
		recentWindow: recentWindow,
		// opencode permission prompts block the tool indefinitely (until you
		// act), while most legitimate tools finish within a few seconds. A
		// persisted permission signal does not exist, so this purely time-based
		// threshold is the only option.
		permissionGrace: permissionGrace,
		pending:         make(map[string]map[string]*pendingTool),
		waiting:         make(map[string]bool),
	}
}

// Kind identifies this source.
func (s *Source) Kind() model.SourceKind { return model.SourceOpenCode }

// Run opens the DB read-only and polls for new event rows until ctx is done.
func (s *Source) Run(ctx context.Context, out chan<- model.Event) error {
	if err := s.open(); err != nil {
		// Don't hard-fail the whole app; the DB may appear later. Retry.
		// We log via the caller by returning nil after ctx ends.
	}
	defer func() {
		if s.db != nil {
			_ = s.db.Close()
		}
	}()

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if s.db == nil {
				if err := s.open(); err != nil {
					continue
				}
			}
			if err := s.poll(ctx, out); err != nil {
				// Reset connection on error; it will be reopened next tick.
				if s.db != nil {
					_ = s.db.Close()
					s.db = nil
				}
			}
			// Promote tools that have been pending past the grace period to a
			// "waiting for permission" state.
			s.checkPermission(out)
		}
	}
}

// open establishes a read-only connection with a busy timeout so concurrent
// writes by opencode don't cause "database is locked" errors.
func (s *Source) open() error {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", s.dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return err
	}
	s.db = db
	return nil
}

// poll reads new events since the cursor and emits normalized events.
//
// On the first poll we seed the cursor near the tail and backfill metadata for
// sessions that were recently active, so the dashboard shows current sessions
// immediately without replaying the entire history.
func (s *Source) poll(ctx context.Context, out chan<- model.Event) error {
	if !s.primed {
		if err := s.prime(ctx, out); err != nil {
			return err
		}
		s.primed = true
		return nil
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, aggregate_id, type, data FROM event WHERE rowid > ? ORDER BY rowid ASC LIMIT 2000`,
		s.cursor)
	if err != nil {
		return err
	}
	defer rows.Close()

	var maxRow int64 = s.cursor
	touched := map[string]bool{}
	for rows.Next() {
		var rowid int64
		var aggID, typ, data string
		if err := rows.Scan(&rowid, &aggID, &typ, &data); err != nil {
			return err
		}
		if rowid > maxRow {
			maxRow = rowid
		}
		if !strings.HasPrefix(aggID, "ses") {
			continue // only session aggregates
		}
		s.handle(aggID, typ, data, out)
		touched[aggID] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.cursor = maxRow

	// Refresh authoritative metadata (tokens/cost/title/model) for sessions
	// that changed in this batch.
	for id := range touched {
		if meta := s.loadSession(ctx, id); meta != nil {
			emit(out, model.Event{Kind: model.EventUpsert, Source: model.SourceOpenCode, ID: id, Session: meta, At: meta.UpdatedAt})
		}
	}
	return nil
}

// prime seeds the cursor and backfills metadata + a little recent activity for
// sessions updated within the recent window.
func (s *Source) prime(ctx context.Context, out chan<- model.Event) error {
	// Current max rowid becomes our starting cursor (we stream forward only).
	var maxRow sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(rowid) FROM event`).Scan(&maxRow); err != nil {
		return err
	}
	if maxRow.Valid {
		s.cursor = maxRow.Int64
	}

	cutoff := time.Now().Add(-s.recentWindow).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, directory, title, agent, model, cost,
		       tokens_input, tokens_output, tokens_reasoning,
		       tokens_cache_read, tokens_cache_write,
		       time_created, time_updated
		FROM session
		WHERE time_updated >= ? AND (time_archived IS NULL)
		ORDER BY time_updated DESC LIMIT 200`, cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return err
		}
		ids = append(ids, sess.ID)
		emit(out, model.Event{Kind: model.EventUpsert, Source: model.SourceOpenCode, ID: sess.ID, Session: sess, At: sess.UpdatedAt})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Detect sessions that are *already* waiting on a question when we start,
	// since the forward-only event tail would otherwise miss the in-flight ask.
	s.detectWaiting(ctx, ids, out)
	return nil
}

// detectWaiting inspects the newest tool part for each session and, if the
// agent is currently blocked on the user, emits a wait-begin so a prompt issued
// before the monitor started is surfaced immediately. Two cases:
//   - a `question` tool that is running/pending (asking a question), or
//   - any other tool stuck running/pending with no end time (awaiting a
//     permission approval).
func (s *Source) detectWaiting(ctx context.Context, ids []string, out chan<- model.Event) {
	cutoff := time.Now().Add(-s.recentWindow)
	for _, id := range ids {
		// Most recent parts for this session; the first tool part we encounter
		// (newest-first) reflects the current tool state.
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, data, time_updated FROM part WHERE session_id = ? ORDER BY time_updated DESC LIMIT 60`, id)
		if err != nil {
			continue
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var partID, data string
				var tu int64
				if err := rows.Scan(&partID, &data, &tu); err != nil {
					return
				}
				var p struct {
					Type   string `json:"type"`
					Tool   string `json:"tool"`
					CallID string `json:"callID"`
					State  struct {
						Status string          `json:"status"`
						Title  string          `json:"title"`
						Input  json.RawMessage `json:"input"`
						Time   struct {
							Start int64 `json:"start"`
							End   int64 `json:"end"`
						} `json:"time"`
					} `json:"state"`
				}
				if err := json.Unmarshal([]byte(data), &p); err != nil {
					continue
				}
				if p.Type != "tool" {
					continue
				}
				at := msToTime(tu)
				if isQuestionTool(p.Tool) {
					if p.State.Status == "running" || p.State.Status == "pending" {
						if w := parseQuestionWait(p.State.Input, at); w != nil {
							emit(out, model.Event{Kind: model.EventWaitBegin, Source: model.SourceOpenCode, ID: id, At: at, Wait: w})
						}
					}
				} else if (p.State.Status == "running" || p.State.Status == "pending") &&
					p.State.Time.End == 0 && at.After(cutoff) {
					// A non-question tool that's unfinished (no end time) => the
					// agent is mid-tool, most likely blocked on a permission
					// prompt. Seed the tracker so checkPermission promotes it
					// after the grace period (and clears it when it resolves).
					key := p.CallID
					if key == "" {
						key = partID
					}
					summary := p.State.Title
					if summary == "" {
						summary = summarizeInput(p.State.Input)
					}
					s.trackPending(id, key, p.Tool, summary, toolStartTime(p.State.Time.Start, at))
				}
				return // stop at the newest tool part
			}
		}()
	}
}

// handle parses one raw event row into normalized events.
func (s *Source) handle(sessionID, typ, data string, out chan<- model.Event) {
	base := strings.TrimSuffix(typ, ".1") // strip schema-version suffix
	switch base {
	case "message.part.updated":
		s.handlePart(sessionID, data, out)
	case "message.updated":
		s.handleMessage(sessionID, data, out)
	case "session.updated":
		// Metadata refresh is handled separately via loadSession; just bump
		// the heartbeat here.
		emit(out, model.Event{Kind: model.EventUpsert, Source: model.SourceOpenCode, ID: sessionID,
			Session: &model.Session{ID: sessionID, Source: model.SourceOpenCode}, At: time.Now()})
	case "session.created":
		emit(out, model.Event{Kind: model.EventUpsert, Source: model.SourceOpenCode, ID: sessionID,
			Session: &model.Session{ID: sessionID, Source: model.SourceOpenCode}, At: time.Now()})
	}
}

func (s *Source) handlePart(sessionID, data string, out chan<- model.Event) {
	var p struct {
		Part struct {
			ID     string          `json:"id"`
			Type   string          `json:"type"`
			Tool   string          `json:"tool"`
			CallID string          `json:"callID"`
			Text   string          `json:"text"`
			State  json.RawMessage `json:"state"`
		} `json:"part"`
		Time int64 `json:"time"`
	}
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return
	}
	at := msToTime(p.Time)
	if at.IsZero() {
		at = time.Now()
	}
	switch p.Part.Type {
	case "tool":
		var st struct {
			Status string          `json:"status"`
			Title  string          `json:"title"`
			Input  json.RawMessage `json:"input"`
			Time   struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			} `json:"time"`
		}
		_ = json.Unmarshal(p.Part.State, &st)
		// Only surface running/pending starts and completions as activity to
		// avoid flooding on every streamed delta.
		summary := st.Title
		if summary == "" {
			summary = summarizeInput(st.Input)
		}

		// The `question` tool blocks the agent on the user. While its part is
		// running/pending the agent is waiting for an answer; when it completes
		// or errors the wait is over. This is the reliable, persisted signal
		// for "waiting for user interaction".
		if isQuestionTool(p.Part.Tool) {
			switch st.Status {
			case "running", "pending":
				if w := parseQuestionWait(st.Input, at); w != nil {
					emit(out, model.Event{Kind: model.EventWaitBegin, Source: model.SourceOpenCode, ID: sessionID, At: at, Wait: w})
				}
			case "completed", "error":
				emit(out, model.Event{Kind: model.EventWaitEnd, Source: model.SourceOpenCode, ID: sessionID, At: at})
			}
		} else {
			// Non-question tools: when opencode prompts for permission to run a
			// tool, the part is written with status `running` (or `pending`) and
			// no end time, and it stays that way until you approve. A tool that
			// stays unfinished past the grace period is treated as awaiting
			// permission; it's cleared once it completes/errors (or an end time
			// appears). Identity is the callID (parts here have no top-level id).
			key := p.Part.CallID
			if key == "" {
				key = p.Part.ID
			}
			if key != "" {
				unfinished := (st.Status == "running" || st.Status == "pending") && st.Time.End == 0
				if unfinished {
					s.trackPending(sessionID, key, p.Part.Tool, summary, toolStartTime(st.Time.Start, at))
				} else {
					s.clearPending(sessionID, key, out)
				}
			}
		}

		switch st.Status {
		case "running", "pending":
			emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceOpenCode, ID: sessionID, At: at,
				Item: &model.Activity{Kind: model.ActivityTool, Tool: p.Part.Tool, Text: summary, At: at}})
		case "completed", "error":
			emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceOpenCode, ID: sessionID, At: at,
				Item: &model.Activity{Kind: model.ActivityTool, Tool: p.Part.Tool, Text: summary, At: at}})
		}
	case "text":
		txt := strings.TrimSpace(p.Part.Text)
		if txt == "" {
			return
		}
		emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceOpenCode, ID: sessionID, At: at,
			Item: &model.Activity{Kind: model.ActivityMessage, Role: model.RoleAssistant, Text: firstLine(txt, 200), At: at}})
	}
}

// trackPending records a tool part currently in `pending` status.
func (s *Source) trackPending(sessionID, partID, tool, summary string, at time.Time) {
	m := s.pending[sessionID]
	if m == nil {
		m = make(map[string]*pendingTool)
		s.pending[sessionID] = m
	}
	if _, ok := m[partID]; !ok {
		since := at
		if since.IsZero() {
			since = time.Now()
		}
		m[partID] = &pendingTool{tool: tool, summary: summary, since: since}
	}
}

// clearPending removes a resolved tool part and, if it was the last pending one
// holding the session in a permission wait, ends the wait.
func (s *Source) clearPending(sessionID, partID string, out chan<- model.Event) {
	m := s.pending[sessionID]
	if m != nil {
		delete(m, partID)
		if len(m) == 0 {
			delete(s.pending, sessionID)
		}
	}
	if s.waiting[sessionID] && len(s.pending[sessionID]) == 0 {
		delete(s.waiting, sessionID)
		emit(out, model.Event{Kind: model.EventWaitEnd, Source: model.SourceOpenCode, ID: sessionID, At: time.Now()})
	}
}

// checkPermission promotes tools that have been pending longer than the grace
// period into a "waiting for permission" state. Called each poll tick.
func (s *Source) checkPermission(out chan<- model.Event) {
	now := time.Now()
	for sessionID, m := range s.pending {
		if s.waiting[sessionID] || len(m) == 0 {
			continue
		}
		var oldest *pendingTool
		for _, pt := range m {
			age := now.Sub(pt.since)
			if age < s.permissionGrace {
				continue
			}
			// Ignore ancient pending parts (e.g. backfilled from an old log);
			// a real permission prompt is recent.
			if age > s.recentWindow {
				continue
			}
			if oldest == nil || pt.since.Before(oldest.since) {
				oldest = pt
			}
		}
		if oldest != nil {
			s.waiting[sessionID] = true
			prompt := "approve " + oldest.tool
			if oldest.summary != "" {
				prompt += ": " + oldest.summary
			}
			w := &model.Wait{
				Kind:   model.WaitPermission,
				Prompt: firstLine(prompt, 240),
				Tool:   oldest.tool,
				Since:  oldest.since,
			}
			emit(out, model.Event{Kind: model.EventWaitBegin, Source: model.SourceOpenCode, ID: sessionID, At: now, Wait: w})
		}
	}
}

func (s *Source) handleMessage(sessionID, data string, out chan<- model.Event) {
	var m struct {
		Info struct {
			Role string `json:"role"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return
	}
	if m.Info.Role == "user" {
		emit(out, model.Event{Kind: model.EventActivity, Source: model.SourceOpenCode, ID: sessionID, At: time.Now(),
			Item: &model.Activity{Kind: model.ActivityMessage, Role: model.RoleUser, Text: "user prompt", At: time.Now()}})
	}
}

// loadSession fetches authoritative session metadata.
func (s *Source) loadSession(ctx context.Context, id string) *model.Session {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, directory, title, agent, model, cost,
		       tokens_input, tokens_output, tokens_reasoning,
		       tokens_cache_read, tokens_cache_write,
		       time_created, time_updated
		FROM session WHERE id = ?`, id)
	sess, err := scanSession(row)
	if err != nil {
		return nil
	}
	return sess
}

// rowScanner unifies *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(r rowScanner) (*model.Session, error) {
	var (
		id, dir, title     string
		agent, modelID     sql.NullString
		cost               float64
		tin, tout, treason int64
		tcr, tcw           int64
		tCreated, tUpdated int64
	)
	if err := r.Scan(&id, &dir, &title, &agent, &modelID, &cost,
		&tin, &tout, &treason, &tcr, &tcw, &tCreated, &tUpdated); err != nil {
		return nil, err
	}
	return &model.Session{
		ID:        id,
		Source:    model.SourceOpenCode,
		Title:     title,
		Directory: dir,
		Agent:     agent.String,
		Model:     parseModel(modelID.String),
		Cost:      cost,
		Tokens: model.Tokens{
			Input: tin, Output: tout, Reasoning: treason,
			CacheRead: tcr, CacheWrite: tcw,
		},
		CreatedAt: msToTime(tCreated),
		UpdatedAt: msToTime(tUpdated),
	}, nil
}

// emit sends an event, dropping it only if the consumer is gone.
func emit(out chan<- model.Event, ev model.Event) {
	defer func() { _ = recover() }() // guard against send on closed channel during shutdown
	out <- ev
}

// parseModel extracts a readable model id from opencode's stored model column,
// which is JSON like {"id":"claude-opus-4.8","providerID":"github-copilot"}.
// Falls back to the raw string if it isn't JSON.
func parseModel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw[0] == '{' {
		var m struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
		}
		if err := json.Unmarshal([]byte(raw), &m); err == nil && m.ID != "" {
			if m.ProviderID != "" {
				return m.ProviderID + "/" + m.ID
			}
			return m.ID
		}
	}
	return raw
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// toolStartTime prefers the tool's own start timestamp (state.time.start) so the
// grace period is measured from when the tool actually began, not when we first
// observed it. Falls back to the event time.
func toolStartTime(startMs int64, fallback time.Time) time.Time {
	if t := msToTime(startMs); !t.IsZero() {
		return t
	}
	return fallback
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// summarizeInput extracts a compact human-readable hint from a tool input blob.
func summarizeInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range []string{"command", "filePath", "file_path", "path", "pattern", "query", "url", "description"} {
		if v, ok := m[k]; ok {
			if str, ok := v.(string); ok && str != "" {
				return firstLine(str, 120)
			}
		}
	}
	return ""
}

// isQuestionTool reports whether a tool name represents asking the user a
// question (which blocks the agent until answered).
func isQuestionTool(tool string) bool {
	switch strings.ToLower(tool) {
	case "question", "ask", "ask_followup_question", "askquestion":
		return true
	}
	return false
}

// parseQuestionWait extracts the question prompt and options from a `question`
// tool's input. opencode's input looks like:
//
//	{"questions":[{"question":"…","header":"…","options":[{"label":"…"},…]}]}
func parseQuestionWait(raw json.RawMessage, at time.Time) *model.Wait {
	if len(raw) == 0 {
		return &model.Wait{Kind: model.WaitQuestion, Prompt: "waiting for your answer", Tool: "question", Since: at}
	}
	var in struct {
		Questions []struct {
			Question string `json:"question"`
			Header   string `json:"header"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || len(in.Questions) == 0 {
		return &model.Wait{Kind: model.WaitQuestion, Prompt: "waiting for your answer", Tool: "question", Since: at}
	}
	q := in.Questions[0]
	prompt := strings.TrimSpace(q.Question)
	if prompt == "" {
		prompt = strings.TrimSpace(q.Header)
	}
	w := &model.Wait{Kind: model.WaitQuestion, Prompt: firstLine(prompt, 240), Tool: "question", Since: at}
	for _, o := range q.Options {
		if l := strings.TrimSpace(o.Label); l != "" {
			w.Options = append(w.Options, firstLine(l, 80))
		}
	}
	// Note if there are multiple questions in the batch.
	if len(in.Questions) > 1 {
		w.Prompt = w.Prompt + " (+" + strconv.Itoa(len(in.Questions)-1) + " more)"
	}
	return w
}
