# hirdman

The daily brief as a task board, served over HTTP. Each task has buttons that open an interactive
agent session on it ([Claude Code](https://claude.com/claude-code) by default), in the task's own project folder, to reply to
someone, act on the task or move its Jira ticket to QA. The session reports back to the board through its API.

One Go binary, standard library plus a TOML parser; the whole page is `index.html`, embedded at build time.

## How it works

- **Tasks** live in one JSON state file. Each belongs to an *area*: a project folder its sessions run in.
  Every change is also appended to a weekly history log (`log/<monday>.jsonl` next to the state file).
- **Buttons** (`reply`, `act`, `qa`) start the session agent (`[use]`) in that area's folder, in a new wezterm mux tab or window
  (or any terminal, see `HIRDMAN_TERMINAL`), with the task as JSON and the rule for that action.
- **The brief** runs on a schedule per area (`briefEvery`) or from the page: each of the area's `runs`, a prompt the
  agent runs in its folder, or the built-in roadmap run that keeps a task per next item of its roadmap files.
- **Watchers**, each optional:
  - Jira: reads the kith room where an area's Jira bot posts, and starts an `update` session for each changed
    ticket.
  - GitHub: reads notifications through `gh`, links pull requests to their tasks, and settles a task once its
    pull requests are merged.
  - Triggers: a task can fire an action when a branch ships, a Jira status is reached or a time comes.

## Requirements

- Go (the version in `go.mod`'s `toolchain` line, which `make` downloads)
- An agent CLI that takes Claude Code's flags: [`claude`](https://claude.com/claude-code) on `PATH` by default
- wezterm, unless `HIRDMAN_TERMINAL` names another terminal
- Optional: `gh` (GitHub watcher), a kith MCP server (Jira watcher)

## Quick start

```sh
make build
./hirdman
```

Then open <http://localhost:8787> and add a project in the settings (⚙). `make run` does the same from source.

## Configuration

Settings live in `~/.config/hirdman/config.toml` (`HIRDMAN_CONFIG`); no file is an empty board. The page's settings
form edits the file and restarts the board. Each setting's env var, when set, wins over the file.

```toml
briefEvery = "6h"
githubPoll = "1m"

[mcp.kith]
command = "kith-mcp"

[mcp.atlassian]
url = "https://mcp.atlassian.com/v2/mcp"

[[areas]]
id = "work"
name = "Work"
dir = "~/dev/work"
mcp = ["kith", "atlassian"]

  [[areas.runs]]
  name = "brief"
  prompt = """Use the brief skill. This is a scheduled run and nobody is watching."""

  [[areas.feeds]]
  mcp = "kith"
  tool = "read_room"
  args = { room = "slack:T0000/D0000", limit = 50 }

[[areas]]
id = "kith"
dir = "~/dev/kith"
roadmap = ["planning/roadmap.md"]
```

An area: `id` (stored on its tasks), `name` (shown), `dir` (where its sessions and runs run), `roadmap` (globs of the
files the roadmap run reads; unset is `roadmap.md`, `TODO.md` and the like, `[]` none), `runs` (what its brief is:
each a `prompt`, or `builtin = "roadmap"`; unset is the roadmap run, `[]` no brief; a prompt's `agent` overrides the
area's `use.headless`), `mcp`, `feeds`, `briefEvery`, `issueUrl`, `prUrl`, `use` (the area's own `[use]` picks).

Agents are `[agents.<name>]` entries of `kind = "claude-code"` (Claude Code, the only kind yet) with a `command` (default
`claude`) and a `model` (its `--model`). `[use]` picks one per job: `session` (the sessions the buttons open: code
investigation and implementation), `headless` (brief runs) and `decide` (the light headless jobs: Jira triage, a Jira
update's refresh of its task, the Jira check of a closed task). Built in, and what your own entries add to or override:

```toml
[agents.haiku]
kind = "claude-code"
model = "haiku"

[agents.opus]
kind = "claude-code"
model = "opus"

[use]
session = "opus"
headless = "opus"
decide = "haiku"
```

`[mcp.<name>]` is an MCP server: a stdio `command` (with `args`), or an HTTP `url`. An area's `mcp` lists the servers its
agents (sessions, Jira updates, triage, its brief runs) get, and only those (`--mcp-config` with `--strict-mcp-config`);
unset leaves them the folder's own Claude config, `[]` gives none. A run's own `mcp` replaces its area's for that run.

An area's `feeds` are its issue updates: each calls `tool` of a stdio `[mcp]` server with `args`, every `jiraPoll`, and
takes the answer as `{"messages": [{"event_id", "body"}]}` (kith's `read_room`). The issue keys in each new message
(`keys`, a pattern; empty is `issueKey`) update the tasks that track them, or are triaged onto the board.

| Key | Env | Default | Meaning |
| --- | --- | --- | --- |
| | `HIRDMAN_ADDR` | `:8787` | Listen address (env only) |
| | `HIRDMAN_STATE` | `~/.local/state/hirdman/state.json` | State file; logs live next to it (env only) |
| | `HIRDMAN_CONFIG` | `~/.config/hirdman/config.toml` | This file (env only) |
| `url` | `HIRDMAN_URL` | `http://localhost` + `HIRDMAN_ADDR` | How sessions reach the board |
| `terminal` | `HIRDMAN_TERMINAL` | wezterm mux | A terminal command the session is appended to, e.g. `kitty`, `alacritty -e` |
| `sessionIn` | `HIRDMAN_SESSION_IN` | `tab` | wezterm: a new `tab` in the last used window, or its own `window` |
| `briefEvery` | `HIRDMAN_BRIEF_EVERY` | off | Default brief interval for areas without `briefEvery` (a Go duration) |
| `jiraPoll` | `HIRDMAN_JIRA_POLL` | `2m` | How often to read each area's `feeds` |
| `githubPoll` | `HIRDMAN_GITHUB_POLL` | off | How often to read GitHub notifications |
| `issueKey` | `HIRDMAN_ISSUE_KEY` | `\b[A-Z][A-Z0-9]+-\d+\b` | Issue key pattern in the issue feeds and GitHub |
| `issueUrl`, `prUrl` | `HIRDMAN_ISSUE_URL`, `HIRDMAN_PR_URL` | none | Link bases for issue keys and `#PR` numbers on the page, for areas without their own; an area without `prUrl` first takes its folder's GitHub `origin` |
| `cleanMerged` | `HIRDMAN_CLEAN_MERGED` | off | `"1"`: a task done with a merged pull request closes its session and removes the worktree |

The comment at the top of `main.go` documents each one in full.

## API

JSON over HTTP; a task update merges the fields it sends.

| Route | Does |
| --- | --- |
| `GET /api/state` | The board: brief, tasks, log, config |
| `GET /api/log` | History from the weekly logs |
| `PUT /api/tasks/{id}` | Create or update a task |
| `POST /api/tasks/{id}/notes` | Add a history entry (`actor`, `kind`, `text`) |
| `POST /api/tasks/{id}/files` | Attach files (multipart `file`) for the next session |
| `POST /api/tasks/{id}/act` | Start a session for an action |
| `PUT /api/brief`, `POST /api/brief/run` | Write the brief, run it now |
| `GET`/`PUT /api/settings` | Read or save settings |

## Development

| Command | Does |
| --- | --- |
| `make test` | Tests with the race detector |
| `make lint` / `make fmt` | golangci-lint / gofmt |
| `make check` | Every CI gate, with CI's toolchain and pinned tools |
| `make hooks` | Enable the pre-commit and pre-push hooks |
| `make deploy` | Build and install to `~/.local/bin` (no gates) |
