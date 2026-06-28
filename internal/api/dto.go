// Package api exposes the status HTTP server: a JSON snapshot endpoint, an SSE
// live stream, and a health check. These let external consumers (status bars,
// web widgets, scripts) display agent activity outside the main TUI.
package api

import (
	"encoding/json"
	"time"

	"github.com/exdis/agent-monitor/internal/model"
	"github.com/exdis/agent-monitor/internal/registry"
)

// statusDTO is the stable, public JSON schema returned by GET /status. It is
// intentionally decoupled from internal model types so refactors don't break
// external consumers.
type statusDTO struct {
	GeneratedAt time.Time    `json:"generatedAt"`
	Counts      countsDTO    `json:"counts"`
	Sessions    []sessionDTO `json:"sessions"`
}

type countsDTO struct {
	Waiting int `json:"waiting"`
	Active  int `json:"active"`
	Idle    int `json:"idle"`
	Ended   int `json:"ended"`
	Stale   int `json:"stale"`
	Total   int `json:"total"`
}

type sessionDTO struct {
	ID            string        `json:"id"`
	Source        string        `json:"source"`
	Title         string        `json:"title,omitempty"`
	Directory     string        `json:"directory,omitempty"`
	Branch        string        `json:"branch,omitempty"`
	Agent         string        `json:"agent,omitempty"`
	Model         string        `json:"model,omitempty"`
	State         string        `json:"state"`
	CurrentAction string        `json:"currentAction,omitempty"`
	Waiting       *waitDTO      `json:"waiting,omitempty"`
	Tokens        tokensDTO     `json:"tokens"`
	Cost          float64       `json:"cost,omitempty"`
	CreatedAt     time.Time     `json:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`
	LastEventAt   time.Time     `json:"lastEventAt"`
	IdleSeconds   int64         `json:"idleSeconds"`
	Recent        []activityDTO `json:"recent,omitempty"`
}

type waitDTO struct {
	Kind         string    `json:"kind"`
	Prompt       string    `json:"prompt,omitempty"`
	Options      []string  `json:"options,omitempty"`
	Tool         string    `json:"tool,omitempty"`
	Since        time.Time `json:"since"`
	WaitingForMs int64     `json:"waitingForMs"`
}

type tokensDTO struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Total      int64 `json:"total"`
}

type activityDTO struct {
	Kind string    `json:"kind"`
	Role string    `json:"role,omitempty"`
	Tool string    `json:"tool,omitempty"`
	Text string    `json:"text,omitempty"`
	At   time.Time `json:"at"`
}

func toStatusDTO(snap registry.Snapshot, includeRecent bool) statusDTO {
	c := snap.Counts()
	dto := statusDTO{
		GeneratedAt: snap.GeneratedAt,
		Counts: countsDTO{
			Waiting: c.Waiting, Active: c.Active, Idle: c.Idle, Ended: c.Ended, Stale: c.Stale,
			Total: len(snap.Sessions),
		},
		Sessions: make([]sessionDTO, 0, len(snap.Sessions)),
	}
	now := time.Now()
	for _, s := range snap.Sessions {
		dto.Sessions = append(dto.Sessions, toSessionDTO(s, now, includeRecent))
	}
	return dto
}

func toSessionDTO(s model.Session, now time.Time, includeRecent bool) sessionDTO {
	var idle int64
	if !s.LastEventAt.IsZero() {
		idle = int64(now.Sub(s.LastEventAt).Seconds())
	}
	d := sessionDTO{
		ID:            s.ID,
		Source:        string(s.Source),
		Title:         s.Title,
		Directory:     s.Directory,
		Branch:        s.Branch,
		Agent:         s.Agent,
		Model:         s.Model,
		State:         string(s.State),
		CurrentAction: s.CurrentAction,
		Tokens: tokensDTO{
			Input: s.Tokens.Input, Output: s.Tokens.Output, Reasoning: s.Tokens.Reasoning,
			CacheRead: s.Tokens.CacheRead, CacheWrite: s.Tokens.CacheWrite, Total: s.Tokens.Total(),
		},
		Cost:        s.Cost,
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
		LastEventAt: s.LastEventAt,
		IdleSeconds: idle,
	}
	if s.Waiting != nil {
		var since int64
		if !s.Waiting.Since.IsZero() {
			since = int64(now.Sub(s.Waiting.Since).Milliseconds())
		}
		d.Waiting = &waitDTO{
			Kind:         string(s.Waiting.Kind),
			Prompt:       s.Waiting.Prompt,
			Options:      s.Waiting.Options,
			Tool:         s.Waiting.Tool,
			Since:        s.Waiting.Since,
			WaitingForMs: since,
		}
	}
	if includeRecent {
		for _, a := range s.Recent {
			d.Recent = append(d.Recent, activityDTO{
				Kind: string(a.Kind), Role: string(a.Role), Tool: a.Tool, Text: a.Text, At: a.At,
			})
		}
	}
	return d
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
