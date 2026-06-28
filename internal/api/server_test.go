package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/exdis/agent-monitor/internal/config"
	"github.com/exdis/agent-monitor/internal/model"
	"github.com/exdis/agent-monitor/internal/registry"
)

func seededRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New(config.Default())
	now := time.Now()
	reg.ApplyForTest(model.Event{
		Kind: model.EventUpsert, Source: model.SourceOpenCode, ID: "s1", At: now,
		Session: &model.Session{ID: "s1", Source: model.SourceOpenCode, Title: "live one",
			Model: "claude", Tokens: model.Tokens{Input: 10, Output: 20}},
	})
	reg.ApplyForTest(model.Event{
		Kind: model.EventActivity, Source: model.SourceOpenCode, ID: "s1", At: now,
		Item: &model.Activity{Kind: model.ActivityTool, Tool: "bash", Text: "ls", At: now},
	})
	reg.ApplyForTest(model.Event{
		Kind: model.EventUpsert, Source: model.SourceCopilot, ID: "c1", At: now,
		Session: &model.Session{ID: "c1", Source: model.SourceCopilot, Title: "copilot one"},
	})
	return reg
}

func TestHandleStatus(t *testing.T) {
	srv := New("127.0.0.1:0", seededRegistry(t))

	req := httptest.NewRequest(http.MethodGet, "/status?recent=1", nil)
	rec := httptest.NewRecorder()
	srv.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d", rec.Code)
	}
	var dto statusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if dto.Counts.Total != 2 {
		t.Fatalf("total = %d, want 2", dto.Counts.Total)
	}
	if dto.Counts.Active != 2 {
		t.Fatalf("active = %d, want 2", dto.Counts.Active)
	}
	// Recent activity included.
	var foundTool bool
	for _, s := range dto.Sessions {
		for _, a := range s.Recent {
			if a.Tool == "bash" {
				foundTool = true
			}
		}
		if s.Tokens.Total != s.Tokens.Input+s.Tokens.Output {
			// only meaningful for the opencode one
		}
	}
	if !foundTool {
		t.Error("expected recent bash activity in status output")
	}
}

func TestHandleStatusSourceFilter(t *testing.T) {
	srv := New("127.0.0.1:0", seededRegistry(t))
	req := httptest.NewRequest(http.MethodGet, "/status?source=copilot", nil)
	rec := httptest.NewRecorder()
	srv.handleStatus(rec, req)

	var dto statusDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &dto)
	if dto.Counts.Total != 1 {
		t.Fatalf("filtered total = %d, want 1", dto.Counts.Total)
	}
	if dto.Sessions[0].Source != "copilot" {
		t.Fatalf("source = %q, want copilot", dto.Sessions[0].Source)
	}
}

func TestHandleHealth(t *testing.T) {
	srv := New("127.0.0.1:0", seededRegistry(t))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("health = %d %q", rec.Code, rec.Body.String())
	}
}
