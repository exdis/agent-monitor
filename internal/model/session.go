// Package model defines the agent-agnostic domain types used across the
// application. Both the OpenCode and Copilot sources normalize their native
// event streams into these types, giving the registry, TUI, and API a single
// shared vocabulary.
package model

import "time"

// SourceKind identifies which coding agent a session belongs to.
type SourceKind string

const (
	SourceOpenCode SourceKind = "opencode"
	SourceCopilot  SourceKind = "copilot"
)

// State is the derived liveness state of a session.
type State string

const (
	// StateActive means the session emitted an event very recently and is not
	// in a terminal state (the agent is actively working).
	StateActive State = "active"
	// StateWaiting means the agent is blocked on the user: a question is
	// pending, or a tool/permission needs approval. Takes priority over
	// active/idle so it's never hidden.
	StateWaiting State = "waiting"
	// StateIdle means the session is alive but has not emitted events recently,
	// or explicitly reported an idle/turn-end with no follow-up work.
	StateIdle State = "idle"
	// StateEnded means a terminal event was observed (shutdown/abort) or the
	// session is known to be finished.
	StateEnded State = "ended"
	// StateStale means no events for a long time without a terminal marker;
	// the producing process likely died without a clean shutdown.
	StateStale State = "stale"
)

// WaitKind categorizes why a session is waiting on the user.
type WaitKind string

const (
	WaitQuestion   WaitKind = "question"   // agent asked a question
	WaitPermission WaitKind = "permission" // a tool/permission needs approval
	WaitApproval   WaitKind = "approval"   // generic approval/confirmation
)

// ActivityKind categorizes a normalized activity entry.
type ActivityKind string

const (
	ActivityMessage ActivityKind = "message" // user or assistant text
	ActivityTool    ActivityKind = "tool"    // a tool/command invocation
	ActivityTurn    ActivityKind = "turn"    // turn lifecycle boundary
	ActivityStatus  ActivityKind = "status"  // status/lifecycle change
	ActivityError   ActivityKind = "error"   // error/abort
)

// Role identifies who produced a message-style activity.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Tokens holds token accounting for a session, when the source exposes it.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

// Total returns a best-effort sum of the primary token counters.
func (t Tokens) Total() int64 { return t.Input + t.Output + t.Reasoning }

// Activity is a single normalized event in a session's timeline. The detail
// pane renders a rolling window of these.
type Activity struct {
	Kind ActivityKind `json:"kind"`
	Role Role         `json:"role,omitempty"`
	// Tool is the tool name for ActivityTool entries.
	Tool string `json:"tool,omitempty"`
	// Text is a human-readable summary (message snippet, tool target, etc.).
	Text string    `json:"text,omitempty"`
	At   time.Time `json:"at"`
}

// Wait describes why a session is currently blocked on the user.
type Wait struct {
	Kind WaitKind `json:"kind"`
	// Prompt is the question/permission text shown to the user.
	Prompt string `json:"prompt,omitempty"`
	// Options lists the choices the user can pick (questions only).
	Options []string `json:"options,omitempty"`
	// Tool is the tool awaiting approval, when applicable.
	Tool  string    `json:"tool,omitempty"`
	Since time.Time `json:"since"`
}

// Session is the unified representation of one agent session, regardless of
// which underlying tool produced it.
type Session struct {
	// ID is unique within a source. Combine with Source for a global key.
	ID     string     `json:"id"`
	Source SourceKind `json:"source"`

	Title     string `json:"title,omitempty"`
	Directory string `json:"directory,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Model     string `json:"model,omitempty"`

	State State `json:"state"`
	// CurrentAction summarizes what the agent appears to be doing right now,
	// e.g. "running tool: bash" or "thinking". Empty when idle/ended.
	CurrentAction string `json:"currentAction,omitempty"`
	// Waiting is set when State == StateWaiting, describing what is blocking.
	Waiting *Wait `json:"waiting,omitempty"`

	Tokens Tokens  `json:"tokens"`
	Cost   float64 `json:"cost,omitempty"`

	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	LastEventAt time.Time `json:"lastEventAt"`

	// Recent holds the most recent activity entries, oldest-first, capped by the
	// registry.
	Recent []Activity `json:"recent,omitempty"`
}

// GlobalKey returns a key unique across all sources.
func (s Session) GlobalKey() string { return string(s.Source) + ":" + s.ID }

// EventKind classifies a source event delivered to the registry.
type EventKind string

const (
	// EventUpsert carries a full or partial session snapshot to merge.
	EventUpsert EventKind = "upsert"
	// EventActivity appends a single activity entry to a session timeline.
	EventActivity EventKind = "activity"
	// EventEnded marks a session terminal.
	EventEnded EventKind = "ended"
	// EventResume revives a previously-ended session (e.g. opencode/copilot
	// resumed it). It clears the terminal state so subsequent events flow.
	EventResume EventKind = "resume"
	// EventWaitBegin marks a session as blocked on the user (Wait is set).
	EventWaitBegin EventKind = "wait_begin"
	// EventWaitEnd clears a session's waiting state (resolved/answered).
	EventWaitEnd EventKind = "wait_end"
)

// Event is what a Source emits to the registry. Sources do their own parsing
// and normalization; the registry only merges and computes liveness.
type Event struct {
	Kind    EventKind
	Source  SourceKind
	ID      string
	Session *Session  // set for EventUpsert (fields present are merged)
	Item    *Activity // set for EventActivity
	Wait    *Wait     // set for EventWaitBegin
	At      time.Time // event timestamp (drives the liveness heartbeat)
}
