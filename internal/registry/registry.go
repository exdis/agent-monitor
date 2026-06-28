// Package registry aggregates events from all sources into a single in-memory
// view of sessions, computes liveness state, prunes old sessions, and
// broadcasts changes to subscribers (the TUI and the status API).
package registry

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/exdis/agent-monitor/internal/config"
	"github.com/exdis/agent-monitor/internal/model"
)

// Snapshot is an immutable point-in-time view of all live/recent sessions.
type Snapshot struct {
	GeneratedAt time.Time
	Sessions    []model.Session
}

// Counts summarizes sessions by state.
type Counts struct {
	Waiting int `json:"waiting"`
	Active  int `json:"active"`
	Idle    int `json:"idle"`
	Ended   int `json:"ended"`
	Stale   int `json:"stale"`
}

// Counts tallies the snapshot by state.
func (s Snapshot) Counts() Counts {
	var c Counts
	for _, ss := range s.Sessions {
		switch ss.State {
		case model.StateWaiting:
			c.Waiting++
		case model.StateActive:
			c.Active++
		case model.StateIdle:
			c.Idle++
		case model.StateEnded:
			c.Ended++
		case model.StateStale:
			c.Stale++
		}
	}
	return c
}

// Registry is concurrency-safe and may be read from many goroutines.
type Registry struct {
	cfg config.Config

	mu       sync.RWMutex
	sessions map[string]*model.Session // keyed by GlobalKey

	subsMu sync.Mutex
	subs   map[int]chan Snapshot
	nextID int
}

// New creates a Registry.
func New(cfg config.Config) *Registry {
	return &Registry{
		cfg:      cfg,
		sessions: make(map[string]*model.Session),
		subs:     make(map[int]chan Snapshot),
	}
}

// Run consumes events from in, recomputes liveness on a ticker, prunes stale
// entries, and broadcasts snapshots. It blocks until ctx is cancelled.
func (r *Registry) Run(ctx context.Context, in <-chan model.Event) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-in:
			if r.apply(ev) {
				r.broadcast()
			}
		case <-tick.C:
			// Periodic recompute handles the time-based transitions
			// (active -> idle -> stale) and pruning even without new events.
			if r.recompute() {
				r.broadcast()
			}
		}
	}
}

// apply merges a single event. Returns true if visible state changed.
func (r *Registry) apply(ev model.Event) bool {
	if ev.ID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	key := string(ev.Source) + ":" + ev.ID
	s := r.sessions[key]
	if s == nil {
		s = &model.Session{ID: ev.ID, Source: ev.Source}
		r.sessions[key] = s
	}

	switch ev.Kind {
	case model.EventUpsert:
		if ev.Session != nil {
			mergeSession(s, ev.Session)
		}
	case model.EventActivity:
		if ev.Item != nil {
			r.appendActivity(s, *ev.Item)
			if a := actionFromActivity(*ev.Item); a != "" {
				s.CurrentAction = a
			}
		}
	case model.EventWaitBegin:
		if ev.Wait != nil {
			w := *ev.Wait
			if w.Since.IsZero() {
				w.Since = ev.At
			}
			s.Waiting = &w
		}
	case model.EventWaitEnd:
		s.Waiting = nil
	case model.EventEnded:
		s.State = model.StateEnded
		s.CurrentAction = ""
		s.Waiting = nil
	}

	if !ev.At.IsZero() && ev.At.After(s.LastEventAt) {
		s.LastEventAt = ev.At
	}
	if s.LastEventAt.After(s.UpdatedAt) {
		s.UpdatedAt = s.LastEventAt
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = s.LastEventAt
	}

	r.computeState(s)
	return true
}

// recompute refreshes time-derived state for all sessions and prunes old ones.
func (r *Registry) recompute() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	now := time.Now()
	for key, s := range r.sessions {
		prev := s.State
		r.computeState(s)
		if s.State != prev {
			changed = true
		}
		// Prune ended/idle/stale sessions that fell outside the recent window.
		// Active and waiting sessions are always kept.
		if s.State != model.StateActive && s.State != model.StateWaiting {
			ref := s.LastEventAt
			if ref.IsZero() {
				ref = s.UpdatedAt
			}
			if !ref.IsZero() && now.Sub(ref) > r.cfg.RecentWindow {
				delete(r.sessions, key)
				changed = true
			}
		}
	}
	return changed
}

// computeState derives liveness from the heartbeat and any terminal marker.
// Caller must hold the write lock.
func (r *Registry) computeState(s *model.Session) {
	if s.State == model.StateEnded {
		s.CurrentAction = ""
		s.Waiting = nil
		return
	}
	// Waiting on the user takes priority over the time-based heartbeat: a
	// pending question/permission is the most important thing to surface, and
	// it shouldn't decay to "idle" just because the user is taking their time.
	if s.Waiting != nil {
		s.State = model.StateWaiting
		s.CurrentAction = ""
		return
	}
	ref := s.LastEventAt
	if ref.IsZero() {
		ref = s.UpdatedAt
	}
	if ref.IsZero() {
		s.State = model.StateIdle
		return
	}
	age := time.Since(ref)
	switch {
	case age <= r.cfg.ActiveThreshold:
		s.State = model.StateActive
	case age <= r.cfg.StaleThreshold:
		s.State = model.StateIdle
		s.CurrentAction = ""
	default:
		s.State = model.StateStale
		s.CurrentAction = ""
	}
}

func (r *Registry) appendActivity(s *model.Session, a model.Activity) {
	s.Recent = append(s.Recent, a)
	if n := len(s.Recent); n > r.cfg.MaxRecent {
		s.Recent = append(s.Recent[:0:0], s.Recent[n-r.cfg.MaxRecent:]...)
	}
}

// Snapshot returns the current sorted view.
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshotLocked()
}

func (r *Registry) snapshotLocked() Snapshot {
	out := make([]model.Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, *s) // copy (Recent slice shared, treated read-only)
	}
	sortSessions(out)
	return Snapshot{GeneratedAt: time.Now(), Sessions: out}
}

// sortSessions orders by state priority (active first), then most recent.
func sortSessions(ss []model.Session) {
	rank := map[model.State]int{
		model.StateWaiting: 0,
		model.StateActive:  1,
		model.StateIdle:    2,
		model.StateStale:   3,
		model.StateEnded:   4,
	}
	sort.SliceStable(ss, func(i, j int) bool {
		ri, rj := rank[ss[i].State], rank[ss[j].State]
		if ri != rj {
			return ri < rj
		}
		return ss[i].LastEventAt.After(ss[j].LastEventAt)
	})
}

// Subscribe returns a channel that receives snapshots on every change, plus an
// unsubscribe func. The channel is buffered and lossy (latest-wins) so slow
// consumers never block the registry.
func (r *Registry) Subscribe() (<-chan Snapshot, func()) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()
	id := r.nextID
	r.nextID++
	ch := make(chan Snapshot, 1)
	r.subs[id] = ch
	// Prime with current state.
	ch <- r.Snapshot()
	return ch, func() {
		r.subsMu.Lock()
		defer r.subsMu.Unlock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
	}
}

func (r *Registry) broadcast() {
	snap := r.Snapshot()
	r.subsMu.Lock()
	defer r.subsMu.Unlock()
	for _, ch := range r.subs {
		// Lossy: drop the stale pending value, push the newest.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- snap:
		default:
		}
	}
}

// mergeSession copies non-zero fields from src into dst.
func mergeSession(dst, src *model.Session) {
	if src.Title != "" {
		dst.Title = src.Title
	}
	if src.Directory != "" {
		dst.Directory = src.Directory
	}
	if src.Branch != "" {
		dst.Branch = src.Branch
	}
	if src.Agent != "" {
		dst.Agent = src.Agent
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.CurrentAction != "" {
		dst.CurrentAction = src.CurrentAction
	}
	if (src.Tokens != model.Tokens{}) {
		dst.Tokens = src.Tokens
	}
	if src.Cost != 0 {
		dst.Cost = src.Cost
	}
	if !src.CreatedAt.IsZero() {
		if dst.CreatedAt.IsZero() || src.CreatedAt.Before(dst.CreatedAt) {
			dst.CreatedAt = src.CreatedAt
		}
	}
	if !src.UpdatedAt.IsZero() && src.UpdatedAt.After(dst.UpdatedAt) {
		dst.UpdatedAt = src.UpdatedAt
	}
	if src.State != "" && dst.State != model.StateEnded {
		dst.State = src.State
	}
}

// actionFromActivity produces a short "current action" string for an activity.
func actionFromActivity(a model.Activity) string {
	switch a.Kind {
	case model.ActivityTool:
		if a.Tool != "" {
			if a.Text != "" {
				return "running " + a.Tool + ": " + a.Text
			}
			return "running " + a.Tool
		}
	case model.ActivityMessage:
		if a.Role == model.RoleAssistant {
			return "responding"
		}
	}
	return ""
}

// ApplyForTest applies a single event synchronously and is intended for use by
// tests in other packages. It bypasses the Run loop.
func (r *Registry) ApplyForTest(ev model.Event) { r.apply(ev) }
