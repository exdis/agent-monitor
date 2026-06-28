// Package source defines the contract every agent data source implements. A
// source discovers sessions for one coding agent and streams normalized events
// onto a channel for the registry to consume.
package source

import (
	"context"

	"github.com/exdis/agent-monitor/internal/model"
)

// Source is a passive, read-only observer of one coding agent's on-disk state.
// Implementations must never spawn or mutate agent processes.
type Source interface {
	// Kind returns the source identifier.
	Kind() model.SourceKind

	// Run starts observing and emits events on out until ctx is cancelled. It
	// blocks until ctx is done or an unrecoverable error occurs. Transient
	// errors should be handled internally (logged/retried), not returned.
	Run(ctx context.Context, out chan<- model.Event) error
}
