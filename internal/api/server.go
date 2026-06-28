package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/exdis/agent-monitor/internal/registry"
)

// Server is the status HTTP server.
type Server struct {
	addr string
	reg  *registry.Registry
	srv  *http.Server
}

// New creates a status server bound to addr.
func New(addr string, reg *registry.Registry) *Server {
	return &Server{addr: addr, reg: reg}
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.addr }

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", s.handleIndex)

	s.srv = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
		return ctx.Err()
	case err := <-errc:
		return err
	}
}

func setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// handleStatus returns a JSON snapshot. Add ?recent=1 to include per-session
// activity timelines.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	includeRecent := truthy(r.URL.Query().Get("recent"))
	snap := s.reg.Snapshot()
	snap = filterSnapshot(snap, r)
	dto := toStatusDTO(snap, includeRecent)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(mustJSON(dto))
}

// handleEvents streams snapshots as Server-Sent Events. Each change pushes a
// fresh full snapshot as a `data:` line; consumers replace their view.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	includeRecent := truthy(r.URL.Query().Get("recent"))
	ch, unsub := s.reg.Subscribe()
	defer unsub()

	// Heartbeat keeps proxies from closing idle connections.
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case snap, ok := <-ch:
			if !ok {
				return
			}
			snap = filterSnapshot(snap, r)
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", mustJSON(toStatusDTO(snap, includeRecent)))
			flusher.Flush()
		case <-hb.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	setCORS(w)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "agent-monitor status API\n\n"+
		"GET /status            JSON snapshot of live/recent sessions\n"+
		"GET /status?recent=1   include per-session activity timelines\n"+
		"GET /status?source=X   filter by source (opencode|copilot)\n"+
		"GET /status?state=X    filter by state (active|idle|ended|stale)\n"+
		"GET /events            SSE stream of snapshots (same query filters)\n"+
		"GET /healthz           health check\n")
}

// filterSnapshot applies optional ?source= and ?state= query filters.
func filterSnapshot(snap registry.Snapshot, r *http.Request) registry.Snapshot {
	src := strings.ToLower(r.URL.Query().Get("source"))
	state := strings.ToLower(r.URL.Query().Get("state"))
	if src == "" && state == "" {
		return snap
	}
	out := snap.Sessions[:0:0]
	for _, s := range snap.Sessions {
		if src != "" && string(s.Source) != src {
			continue
		}
		if state != "" && string(s.State) != state {
			continue
		}
		out = append(out, s)
	}
	snap.Sessions = out
	return snap
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
