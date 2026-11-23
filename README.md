# hirdman

The daily brief as a task board, served over HTTP. Each task has buttons that open an interactive
agent session on it ([Claude Code](https://claude.com/claude-code) by default), in the task's own project folder, to reply to
someone, act on the task or move its Jira ticket to QA. The session reports back to the board through its API.

One Go binary, standard library only; the whole page is `index.html`, embedded at build time.

## How it works

- **Tasks** live in one JSON state file. Each belongs to an *area*: a project folder its sessions run in.
  Every change is also appended to a weekly history log (`log/<monday>.jsonl` next to the state file).
- **Buttons** (`reply`, `act`, `qa`) start the agent (`HIRDMAN_AGENT`) in that area's folder, in a new wezterm mux tab or window
  (or any terminal, see `HIRDMAN_TERMINAL`), with the task as JSON and the rule for that action.
- **The brief** runs on a schedule per area (`briefEvery`) or from the page, and fills the board from the
  area's sources and roadmap files.
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
HIRDMAN_AREAS='[{"id":"board","name":"Hirdman","dir":"~/dev/hirdman"}]' ./hirdman
```

Then open <http://localhost:8787>. `make run` does the same from source.

## Configuration

Settings come from the environment. Those the page's settings form edits are saved in `settings.json`
next to the state file, which wins over the environment; saving them restarts the board.

| Variable | Default | Meaning |
| --- | --- | --- |
| `HIRDMAN_AREAS` | required | JSON list of areas: `{"id", "name", "dir", "roadmap", "jiraRoom", "briefEvery", "issueUrl", "prUrl"}`; the settings form edits it field by field |
| `HIRDMAN_ADDR` | `:8787` | Listen address |
| `HIRDMAN_STATE` | `~/.local/state/hirdman/state.json` | State file; logs and `settings.json` live next to it |
| `HIRDMAN_URL` | `http://localhost` + `HIRDMAN_ADDR` | How sessions reach the board |
| `HIRDMAN_AGENT` | `claude` | The agent command; it takes Claude Code's flags |
| `HIRDMAN_TERMINAL` | wezterm mux | A terminal command the session is appended to, e.g. `kitty`, `alacritty -e` |
| `HIRDMAN_SESSION_IN` | `tab` | wezterm: a new `tab` in the last used window, or its own `window` |
| `HIRDMAN_BRIEF_EVERY` | off | Default brief interval for areas without `briefEvery` (a Go duration) |
| `HIRDMAN_JIRA_POLL` | `2m` | How often to read each area's `jiraRoom` |
| `HIRDMAN_KITH_MCP` | `kith-mcp` | The kith MCP server command |
| `HIRDMAN_GITHUB_POLL` | off | How often to read GitHub notifications |
| `HIRDMAN_ISSUE_KEY` | `\b[A-Z][A-Z0-9]+-\d+\b` | Issue key pattern in the Jira feed and GitHub |
| `HIRDMAN_ISSUE_URL`, `HIRDMAN_PR_URL` | none | Link bases for issue keys and `#PR` numbers on the page, for areas without `issueUrl` / `prUrl`; an area without `prUrl` first takes its folder's GitHub `origin` |
| `HIRDMAN_CLEAN_MERGED` | off | `1`: a task done with a merged pull request closes its session and removes the worktree |

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
