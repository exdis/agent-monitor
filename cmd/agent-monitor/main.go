// Command agent-monitor is a passive, read-only TUI dashboard that monitors
// running OpenCode and GitHub Copilot CLI sessions and exposes their status
// over an HTTP API (JSON snapshot + SSE stream).
//
// It never spawns or modifies agent processes; it only observes their existing
// on-disk event streams (opencode's SQLite event log and copilot's per-session
// events.jsonl files).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/exdis/agent-monitor/internal/api"
	"github.com/exdis/agent-monitor/internal/config"
	"github.com/exdis/agent-monitor/internal/model"
	"github.com/exdis/agent-monitor/internal/registry"
	"github.com/exdis/agent-monitor/internal/source"
	"github.com/exdis/agent-monitor/internal/source/copilot"
	"github.com/exdis/agent-monitor/internal/source/opencode"
	"github.com/exdis/agent-monitor/internal/tui"
)

func main() {
	cfg := config.Default()

	var (
		sourcesCSV string
		headless   bool
		showVer    bool
	)
	flag.StringVar(&cfg.APIAddr, "api-addr", cfg.APIAddr, "listen address for the status HTTP API")
	noAPI := flag.Bool("no-api", false, "disable the status HTTP API")
	flag.StringVar(&sourcesCSV, "sources", "opencode,copilot", "comma-separated sources to enable")
	flag.BoolVar(&headless, "headless", false, "run only the status API (no TUI)")
	flag.DurationVar(&cfg.ActiveThreshold, "active-threshold", cfg.ActiveThreshold, "max event age to consider a session active")
	flag.DurationVar(&cfg.StaleThreshold, "stale-threshold", cfg.StaleThreshold, "event age after which an idle session becomes stale")
	flag.DurationVar(&cfg.RecentWindow, "recent-window", cfg.RecentWindow, "how long ended/idle sessions remain visible")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", cfg.PollInterval, "polling cadence for file/db tailing")
	flag.DurationVar(&cfg.PermissionGrace, "permission-grace", cfg.PermissionGrace, "how long a tool may run before it's treated as waiting for permission/approval")
	flag.StringVar(&cfg.OpenCodeDB, "opencode-db", "", "override path to opencode-stable.db (default: auto-discover)")
	flag.StringVar(&cfg.CopilotDir, "copilot-dir", "", "override path to copilot session-state dir (default: auto-discover)")
	flag.BoolVar(&showVer, "version", false, "print version and exit")
	flag.Parse()

	if showVer {
		fmt.Println("agent-monitor", version)
		return
	}

	cfg.EnableAPI = !*noAPI
	cfg.Sources = splitCSV(sourcesCSV)

	if err := run(cfg, headless); err != nil {
		log.Fatalf("agent-monitor: %v", err)
	}
}

var version = "0.1.0"

func run(cfg config.Config, headless bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Graceful shutdown on signal.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		cancel()
	}()

	reg := registry.New(cfg)
	events := make(chan model.Event, 1024)

	var wg sync.WaitGroup

	// Registry consumer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		reg.Run(ctx, events)
	}()

	// Sources.
	srcs := buildSources(cfg)
	if len(srcs) == 0 {
		return fmt.Errorf("no sources enabled (use --sources)")
	}
	for _, s := range srcs {
		wg.Add(1)
		go func(s source.Source) {
			defer wg.Done()
			if err := s.Run(ctx, events); err != nil && ctx.Err() == nil {
				log.Printf("source %s stopped: %v", s.Kind(), err)
			}
		}(s)
	}

	// Status API.
	if cfg.EnableAPI {
		srv := api.New(cfg.APIAddr, reg)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("api server error: %v", err)
			}
		}()
	}

	if headless {
		apiInfo := "disabled"
		if cfg.EnableAPI {
			apiInfo = "http://" + cfg.APIAddr
		}
		log.Printf("agent-monitor running headless | sources=%v | api=%s", cfg.Sources, apiInfo)
		<-ctx.Done()
		// Give goroutines a moment to unwind.
		drain(&wg, 2*time.Second)
		return nil
	}

	// Interactive TUI (blocks until quit).
	apiAddr := ""
	if cfg.EnableAPI {
		apiAddr = cfg.APIAddr
	}
	p := tui.Program(ctx, reg, apiAddr)
	_, err := p.Run()
	cancel()
	drain(&wg, 2*time.Second)
	return err
}

func buildSources(cfg config.Config) []source.Source {
	var srcs []source.Source
	if cfg.HasSource("opencode") {
		dbPath := config.OpenCodeDBPath(cfg.OpenCodeDB)
		srcs = append(srcs, opencode.New(dbPath, cfg.PollInterval, cfg.RecentWindow, cfg.PermissionGrace))
	}
	if cfg.HasSource("copilot") {
		dir := config.CopilotStateDir(cfg.CopilotDir)
		srcs = append(srcs, copilot.New(dir, cfg.PollInterval, cfg.RecentWindow))
	}
	return srcs
}

// drain waits for the wait group or times out, so shutdown never hangs.
func drain(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if c != ' ' {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
