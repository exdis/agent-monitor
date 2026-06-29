# agent-monitor

A passive, read-only **TUI dashboard** that monitors running **OpenCode** and
**GitHub Copilot CLI** coding-agent sessions in real time, and exposes their
status over an **HTTP API** (JSON snapshot + SSE stream) so you can display
agent activity anywhere outside the main app (status bars, web widgets,
scripts).

It never spawns or modifies agent processes. It only observes their existing
on-disk state:

- **OpenCode** — tails the append-only `event` table in opencode's live
  SQLite database (`opencode-stable.db`, WAL mode). Read-only access works
  while opencode is running, so sessions started in any terminal are visible.
- **Copilot** — watches `~/.copilot/session-state/<id>/events.jsonl` (one
  append-only event log per session) plus `workspace.yaml` for context.

## Features

- Live session list with per-source badges, liveness state, current action,
  model, idle time, tokens and cost.
- Detail pane with session metadata and a rolling activity feed (tool calls,
  messages, turns, errors).
- **"Waiting for you" detection**: surfaces sessions blocked on a question or a
  tool/permission approval as a distinct, attention-grabbing `waiting` state
  (sorted to the top), showing the actual question and its options.
- Liveness via a **recent-event heartbeat**: `waiting` → `active` → `idle` →
  `stale`, plus explicit `ended` on terminal events. Old sessions are pruned
  automatically; waiting sessions are never pruned.
- Filters by source and state; pause live updates; keyboard-driven.
- Status HTTP API for external consumers.

## Build & run

Requires Go 1.24+.

```sh
go build -o bin/agent-monitor ./cmd/agent-monitor

# Interactive TUI (also starts the status API on 127.0.0.1:7654)
./bin/agent-monitor

# Headless: status API only, no TUI (handy for a long-running background service)
./bin/agent-monitor --headless
```

### Keybindings

| Key            | Action                                            |
| -------------- | ------------------------------------------------- |
| `↑`/`k` `↓`/`j`| move selection (or scroll detail when focused)    |
| `tab`          | switch focus between list and detail              |
| `g` / `G`      | jump to first / last session                      |
| `s`            | cycle source filter (all → opencode → copilot)    |
| `f`            | cycle state filter (all → waiting → active → idle → stale → ended) |
| `p`            | pause / resume live updates                       |
| `?`            | toggle help                                       |
| `q`, `ctrl+c`  | quit                                              |

### Session states

| State     | Meaning                                                              |
| --------- | ------------------------------------------------------------------- |
| `waiting` | Blocked on **you** — a question or tool/permission approval is pending. Sorted to the top; shows the question + options. |
| `active`  | Emitted an event within `--active-threshold` (working right now).    |
| `idle`    | Alive but quiet for a while.                                         |
| `stale`   | No events past `--stale-threshold`; the process may have died.       |
| `ended`   | A terminal event was seen (shutdown/abort).                          |

How "waiting" is detected:

- **OpenCode** — two cases, both read straight from the persisted event log
  (works passively, no server):
  - a `question` tool whose part is `running`/`pending` carries the prompt and
    options (kind `question`); resolved when it `completes`.
  - any other tool that stays unfinished (status `running`/`pending` with no
    end time) longer than `--permission-grace` (default 20s) is treated as
    blocked on a permission/approval prompt (kind `permission`); resolved when
    it completes/errors. OpenCode does **not** persist any permission-request
    signal — a blocked tool is byte-identical to a normally-running one — so
    this is purely time-based and a genuinely slow tool may show as `waiting`.
    Raise `--permission-grace` to trade detection latency for fewer false
    positives.
- **Copilot** — a `permission.requested` event (directory/file access, shell,
  write, url, etc.) surfaces immediately as `waiting` and clears on
  `permission.completed`. As a fallback for sessions without those events, a
  `tool.execution_start` that hasn't `complete`d within a short grace window is
  also treated as `waiting`; resolved on completion, turn end, or shutdown. A
  resumed session (`session.resume` after `session.shutdown`) is revived rather
  than left as `ended`.

## Status API

Default address: `http://127.0.0.1:7654` (change with `--api-addr`, disable
with `--no-api`).

| Endpoint                 | Description                                        |
| ------------------------ | ------------------------------------------------- |
| `GET /status`            | JSON snapshot of live/recent sessions             |
| `GET /status?recent=1`   | include per-session activity timelines            |
| `GET /status?source=X`   | filter by source (`opencode` \| `copilot`)        |
| `GET /status?state=X`    | filter by state (`active`/`idle`/`ended`/`stale`) |
| `GET /events`            | SSE stream of snapshots (same query filters)      |
| `GET /healthz`           | health check                                      |

### `/status` response shape

```json
{
  "generatedAt": "2026-06-28T14:13:45Z",
  "counts": { "waiting": 1, "active": 1, "idle": 0, "ended": 1, "stale": 6, "total": 9 },
  "sessions": [
    {
      "id": "ses_…",
      "source": "opencode",
      "title": "Restoring wallpaper after flake update",
      "directory": "/home/u/.nixos",
      "branch": "main",
      "agent": "build",
      "model": "github-copilot/claude-opus-4.8",
      "state": "waiting",
      "waiting": {
        "kind": "question",
        "prompt": "Which deployment target should I use for the release build?",
        "options": ["Production (Recommended)", "Staging"],
        "tool": "question",
        "since": "…",
        "waitingForMs": 12948
      },
      "tokens": { "input": 9, "output": 2363, "reasoning": 0,
                  "cacheRead": 64564, "cacheWrite": 21908, "total": 2372 },
      "cost": 0.2283,
      "createdAt": "…", "updatedAt": "…", "lastEventAt": "…",
      "idleSeconds": 3,
      "recent": [
        { "kind": "tool", "tool": "bash", "text": "go test ./...", "at": "…" }
      ]
    }
  ]
}
```

The `waiting` object is present only when `state == "waiting"`. `kind` is one of
`question`, `permission`, or `approval`.

### Example: a glanceable status-bar widget

```sh
# Anything waiting on me right now? (e.g. for waybar/polybar urgency)
curl -s localhost:7654/status | jq '.counts.waiting'

# Show the pending question(s)
curl -s localhost:7654/status?state=waiting \
  | jq -r '.sessions[] | "\(.source): \(.waiting.prompt)"'

# Count of currently-active agents
curl -s localhost:7654/status | jq '.counts.active'

# One-liner of what each active agent is doing
curl -s localhost:7654/status?state=active \
  | jq -r '.sessions[] | "\(.source): \(.currentAction // "thinking")"'
```

### Example: live stream

```sh
curl -sN localhost:7654/events
# event: status
# data: {"generatedAt":…,"counts":…,"sessions":[…]}
```

## Configuration

All options are CLI flags (run `--help` for the full list):

| Flag                 | Default            | Description                                   |
| -------------------- | ------------------ | --------------------------------------------- |
| `--api-addr`         | `127.0.0.1:7654`   | status API listen address                     |
| `--no-api`           | off                | disable the status API                        |
| `--headless`         | off                | run only the API (no TUI)                      |
| `--sources`          | `opencode,copilot` | comma-separated sources to enable             |
| `--active-threshold` | `30s`              | max event age to be considered active         |
| `--stale-threshold`  | `5m`               | event age after which idle becomes stale      |
| `--recent-window`    | `15m`              | how long ended/idle sessions stay visible     |
| `--poll-interval`    | `750ms`            | file/db tailing cadence                        |
| `--permission-grace` | `20s`              | tool runtime before it's treated as waiting for permission |
| `--opencode-db`      | auto-discover      | override path to `opencode-stable.db`         |
| `--copilot-dir`      | auto-discover      | override path to copilot `session-state` dir  |

### Path discovery

- **OpenCode DB**: `$OPENCODE_DATA`, then `$XDG_DATA_HOME/opencode`, then
  `~/.local/share/opencode` (and macOS `~/Library/Application Support/opencode`),
  file `opencode-stable.db` or `opencode.db` (most-recently-modified wins).
- **Copilot state**: `$COPILOT_CONFIG_DIR/session-state`, else
  `~/.copilot/session-state`.

## Architecture

```
cmd/agent-monitor      CLI entrypoint, lifecycle wiring
internal/model         agent-agnostic domain types (Session, Activity, Event)
internal/source        Source interface (read-only observer contract)
  └ opencode           SQLite event-table tailer + DB discovery
  └ copilot            session-state watcher + events.jsonl tailer
internal/registry      merges all sources, computes liveness, prunes, broadcasts
internal/tui           Bubble Tea dashboard (list + detail panes)
internal/api           status HTTP server (/status, /events, /healthz)
```

Each source runs a goroutine emitting normalized `model.Event`s onto a channel.
The registry maintains the single in-memory view and fans out snapshots to two
consumers: the TUI (via Bubble Tea messages) and the API (via SSE). Adding a
third agent later is just a new `Source` implementation.

## Notes & limitations

- Read-only by design; it cannot send input to agents (it shows that a session
  is waiting and what it's asking, but you answer in the agent's own terminal).
- Liveness is heuristic: a process killed without a terminal event transitions
  `active → idle → stale` and is eventually pruned.
- Both agents' `waiting` state is partly timing-based: a permission-gated tool
  is detected by how long it sits unresolved, so a genuinely long-running tool
  may briefly show as `waiting`. OpenCode's `question`-tool waits are exact
  (read from the persisted tool state); permission waits use the grace window.
- Parsing targets the agents' current on-disk formats (OpenCode 1.17.x,
  Copilot CLI 1.0.x). Unknown event types are ignored rather than fatal, but
  schema changes in those tools may require updates.
