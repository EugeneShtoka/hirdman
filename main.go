// hirdman serves the daily brief as a task board and opens an interactive agent session
// (wezterm mux, remote control) on a task when a button is pressed (reply, act, qa). Drop is handled here.
// The agent command takes Claude Code's flags, hooks and remote control (see spawnSession, runAgent).
//
// Config: HIRDMAN_CONFIG (~/.config/hirdman/config.toml), see Config: the MCP servers, [mcp.<name>] with "command" and
// "args" or "url", see MCPServer; the board's areas, [[areas]] with "id", "name", "dir", "roadmap", "mcp", "briefEvery",
// "issueUrl", "prUrl", [areas.use], [[areas.runs]] and [[areas.feeds]], the board's areas and the folders their sessions
// run in; roadmap: the files the roadmap run reads, see defaultRoadmap; mcp: the [mcp] servers its agents get, and only
// those, unset is the folder's own; briefEvery: how often the area is briefed, a Go duration, default briefEvery, unset or
// 0 only by the board's button, see scheduleBrief; issueUrl / prUrl: the area's link bases, default issueUrl / prUrl; prUrl
// first defaults to its folder's GitHub origin; use: the agents its jobs run on, over the board's [use]; runs: what its
// brief is, see Run; feeds: the MCP tools whose messages are its issue updates, see Feed. [agents.<name>] and [use]: the
// agents and which one each job runs on, see Agent and Use. And the board's settings, each of which its env var, set, wins
// over: briefEvery (HIRDMAN_BRIEF_EVERY), jiraPoll (HIRDMAN_JIRA_POLL: how often feeds are read), githubPoll
// (HIRDMAN_GITHUB_POLL), issueKey (HIRDMAN_ISSUE_KEY),
// url (HIRDMAN_URL: how the agent reaches us),
// terminal (HIRDMAN_TERMINAL: where sessions open: unset is the wezterm mux, else a command the session is appended to,
// run in the task's folder, e.g. "kitty", "alacritty -e" or "kitty @ launch --type=tab"),
// sessionIn (HIRDMAN_SESSION_IN: tab | window: a wezterm session's new tab in the window last used, or its own window),
// issueUrl / prUrl (HIRDMAN_ISSUE_URL / HIRDMAN_PR_URL: link bases for issue keys and #PR numbers in the page, for areas
// without their own), cleanMerged (HIRDMAN_CLEAN_MERGED: "1": a task moving to done with a merged pull request closes its
// session and removes the pull request's worktree, see cleanMerged).
// Env only: HIRDMAN_ADDR (:8787), HIRDMAN_STATE (~/.local/state/hirdman/state.json).
// The page's settings form edits config.toml, see handleSaveSettings.
// Every history entry is also appended to log/<monday>.jsonl next to the state file, one file per week.
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

//go:embed index.html
var indexHTML []byte

type Entry struct {
	At    time.Time `json:"at"`
	Actor string    `json:"actor"` // eugene | agent
	Kind  string    `json:"kind"`  // note | reply | clarify | act | drop | status
	Text  string    `json:"text"`
}

// LogEntry is one line of a week file: an entry and the task it belongs to (none for board-level).
type LogEntry struct {
	Entry
	Task  string `json:"task,omitempty"`
	Title string `json:"title,omitempty"`
	Area  string `json:"area,omitempty"` // the task's area when it was written
}

type Job struct {
	Action    string     `json:"action"`
	Status    string     `json:"status"` // running | ok | failed | stopped
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Session   string     `json:"session,omitempty"`   // button jobs: the agent session's name, for remote control
	Pane      string     `json:"pane,omitempty"`      // button jobs: its wezterm pane id
	SessionID string     `json:"sessionId,omitempty"` // button jobs: the agent session's id, to resume it by; the task's next session resumes it too
	Retries   int        `json:"retries,omitempty"`   // how often the board resumed or continued the session, see maxRetries
	Rules     []string   `json:"rules,omitempty"`     // button jobs: the actions whose rules the session was given, so a press into it repeats none
	// Question is the AskUserQuestion input the session waits on Eugene to answer on the board, see handleAsk.
	Question json.RawMessage `json:"question,omitempty"`
}

type Task struct {
	ID         string              `json:"id"`
	Title      string              `json:"title"`
	Group      string              `json:"group"`            // waiting_on_you | ready | roadmap; urgency and status say what is urgent, later or waiting
	Status     string              `json:"status"`           // todo | doing | waiting | done | dropped
	Area       string              `json:"area,omitempty"`   // an id from config.toml's areas, required
	Source     string              `json:"source,omitempty"` // who maintains it: "" the brief | eugene | sweep (mail, calendar, personal chats) | roadmap (an area's roadmap files)
	Who        string              `json:"who,omitempty"`
	WaitingFor string              `json:"waitingFor,omitempty"` // what is owed between Eugene and who: a review, an email reply, a Jira answer, info
	Since      string              `json:"since,omitempty"`
	Order      int                 `json:"order,omitempty"`
	Summary    string              `json:"summary,omitempty"`    // the story: what was asked, by whom, what's known
	Tags       []string            `json:"tags,omitempty"`       // Eugene's own labels, set on the board
	Urgency    string              `json:"urgency,omitempty"`    // high | medium | low, medium unless set
	Importance string              `json:"importance,omitempty"` // high | medium | low, medium unless set
	Jira       []string            `json:"jira,omitempty"`
	JiraStatus map[string]string   `json:"jiraStatus,omitempty"` // key -> Jira status name, written by the brief
	Links      []map[string]string `json:"links,omitempty"`      // {label, url}
	Shipping   bool                `json:"shipping,omitempty"`   // its merged pull requests are not on the default branch yet; each GitHub poll re-checks it
	Thread     map[string]string   `json:"thread,omitempty"`     // where a reply goes: {room, reply_to, thread}
	Messages   []map[string]string `json:"messages,omitempty"`   // the story's messages: {label, room, event, url}, opened at url, else in kith
	Files      []string            `json:"files,omitempty"`      // Eugene's attachments: absolute paths under files/<id> next to the state file
	Triggers   []Trigger           `json:"triggers,omitempty"`   // what Eugene wants done once something happens, see fireTriggers
	DoneAt     *time.Time          `json:"doneAt,omitempty"`
	UpdatedAt  time.Time           `json:"updatedAt"`
	History    []Entry             `json:"history"`
	Job        *Job                `json:"job,omitempty"`
}

// Trigger is a session Eugene sets up ahead: once a pull request the task links reaches branch On, one of the
// task's Jira keys reaches status JiraStatus, or time At comes (exactly one is set), a session starts on the task to do Do, once.
type Trigger struct {
	On         string     `json:"on,omitempty"` // a branch, e.g. development or main
	JiraStatus string     `json:"jiraStatus,omitempty"`
	At         *time.Time `json:"at,omitempty"`
	Do         string     `json:"do"`
	Fired      *time.Time `json:"fired,omitempty"`
}

type State struct {
	Brief map[string]any   `json:"brief"`
	Tasks map[string]*Task `json:"tasks"`
	// Log is board-level activity (Jira triage, scheduled briefs), newest last, capped at logCap.
	Log []Entry `json:"log,omitempty"`
	// JiraSeen is, per issue feed (Feed.id), the event ids of its last read; a feed with none yet sets a baseline.
	JiraSeen map[string][]string `json:"jiraSeenIn,omitempty"`
	// GitHubSince is the newest GitHub notification's updated_at seen; empty until the first poll sets a baseline.
	GitHubSince string `json:"githubSince,omitempty"`
	// GitHubOwnSince is the newest of the gh user's own GitHub events seen; empty until the first poll sets a baseline.
	GitHubOwnSince string `json:"githubOwnSince,omitempty"`
	// Briefs is, per area id, its last brief run.
	Briefs map[string]*Job `json:"briefs,omitempty"`
}

const logCap = 200

type Board struct {
	mu       sync.Mutex
	path     string
	logDir   string
	st       State
	home     string
	terminal []string // HIRDMAN_TERMINAL; empty is the wezterm mux
	window   bool     // wezterm: a session gets its own window, not a tab
	selfURL  string
	triage   chan struct{}     // one Jira triage at a time
	prepping sync.Mutex        // one task preprocessing at a time, see prep
	wake     chan struct{}     // nudges scheduleBrief out of its sleep
	rerun    map[string]bool   // area ids to brief now: the board's button, or a brief that ran while kith was down
	ghETag   string            // pollGitHub's own: the ETag of its last notifications answer
	ghOwn    string            // pollOwn's own: the ETag of its last events answer
	ghLogin  string            // the gh user, read once by pollOwn
	clean    bool              // HIRDMAN_CLEAN_MERGED
	areas    []Area            // config.toml's, parsed
	conf     Config            // config.toml as read, which the settings page edits
	confPath string            // HIRDMAN_CONFIG
	env      map[string]string // the service's env, before config.toml; a setting it has wins over the file
	restart  chan struct{}     // a saved setting: main restarts the board to apply it

	asks map[string]chan []byte // task id -> its session's waiting question, which takes the hook's reply
}

// areaRe keeps an area id a plain lowercase name.
var areaRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Area is one area of the board: its id (what a task stores), the name the page shows, the folder its sessions run in,
// the globs, relative to that folder, of the files the roadmap run reads for its next items, the runs its brief is, the
// MCP servers its agents get (nil: the folder's own), the feeds of its issue updates, and how often it is briefed.
type Area struct {
	ID         string        `json:"id" toml:"id"`
	Name       string        `json:"name" toml:"name,omitempty"`
	Dir        string        `json:"dir" toml:"dir"`
	Roadmap    []string      `json:"roadmap" toml:"roadmap"`
	Runs       []Run         `json:"runs" toml:"runs"`
	MCP        []string      `json:"mcp" toml:"mcp"`
	Feeds      []Feed        `json:"feeds,omitempty" toml:"feeds,omitempty"`
	BriefEvery string        `json:"briefEvery,omitempty" toml:"briefEvery,omitempty"`
	IssueURL   string        `json:"issueUrl,omitempty" toml:"issueUrl,omitempty"`
	PRURL      string        `json:"prUrl,omitempty" toml:"prUrl,omitempty"`
	Use        Use           `json:"use,omitzero" toml:"use,omitempty"`
	every      time.Duration // BriefEvery, parsed; 0 is never on schedule
}

// Run is one agent run of an area's brief: Prompt, run in the area's folder, or the built-in "roadmap" run, which keeps a
// task per next item of the area's roadmap files. Name is what the board's log calls it. Agent, set, is the agent a prompt
// runs on, over the area's use.headless. MCP, set, is the servers a prompt run gets instead of its area's; the roadmap run,
// shared by the areas due together, gets the board's folder's own.
type Run struct {
	Name    string   `json:"name,omitempty" toml:"name,omitempty"`
	Prompt  string   `json:"prompt,omitempty" toml:"prompt,omitempty"`
	Builtin string   `json:"builtin,omitempty" toml:"builtin,omitempty"`
	Agent   string   `json:"agent,omitempty" toml:"agent,omitempty"`
	MCP     []string `json:"mcp" toml:"mcp"`
}

// Feed is one of an area's issue feeds: Tool of the [mcp] server MCP, called with Args, which answers
// {"messages": [{"event_id", "body"}]}. The issue keys (Keys, a pattern; empty is issueKey) in each new message are an update
// to the tasks that track them, or, untracked, one to triage; see watchFeed.
type Feed struct {
	MCP  string         `json:"mcp" toml:"mcp"`
	Tool string         `json:"tool" toml:"tool"`
	Args map[string]any `json:"args,omitempty" toml:"args,omitempty"`
	Keys string         `json:"keys,omitempty" toml:"keys,omitempty"`
}

// id names f in the state's seen events: its server, tool and args (fmt sorts the keys).
func (f *Feed) id() string {
	return f.MCP + " " + f.Tool + " " + fmt.Sprint(f.Args)
}

// MCPServer is one [mcp.<name>] server: a stdio Command with its Args, or a streamable HTTP URL. The board calls stdio ones
// itself (feeds); agents get either kind.
type MCPServer struct {
	Command string   `toml:"command,omitempty"`
	Args    []string `toml:"args,omitempty"`
	URL     string   `toml:"url,omitempty"`
}

// mcpFlags are the agent's flags that give it exactly the [mcp] servers names lists; nil is none, the folder's own config.
func (b *Board) mcpFlags(names []string) []string {
	if names == nil {
		return nil
	}
	type server struct {
		Type    string   `json:"type"`
		Command string   `json:"command,omitempty"`
		Args    []string `json:"args,omitempty"`
		URL     string   `json:"url,omitempty"`
	}
	servers := map[string]server{}
	for _, n := range names {
		if s := b.conf.MCP[n]; s.URL != "" {
			servers[n] = server{Type: "http", URL: s.URL}
		} else {
			servers[n] = server{Type: "stdio", Command: s.Command, Args: s.Args}
		}
	}
	data, _ := json.Marshal(struct { //nolint:errchkjson // strings only
		Servers map[string]server `json:"mcpServers"`
	}{servers})
	return []string{"--strict-mcp-config", "--mcp-config", string(data)}
}

// mcpFor is mcpFlags for the area id's servers.
func (b *Board) mcpFor(area string) []string {
	if i := slices.IndexFunc(b.areas, func(a Area) bool { return a.ID == area }); i >= 0 {
		return b.mcpFlags(b.areas[i].MCP)
	}
	return nil
}

// Agent is one [agents.<name>]: kind "claude-code", the only kind yet, is Claude Code, run as Command (empty is claude) on
// Model (its --model; empty is its own default).
type Agent struct {
	Kind    string `toml:"kind"`
	Command string `toml:"command,omitempty"`
	Model   string `toml:"model,omitempty"`
}

// argv is a's command line for a job's flags.
func (a Agent) argv(flags ...string) []string {
	argv := slices.Concat([]string{cmp.Or(a.Command, "claude")}, flags)
	if a.Model != "" {
		argv = append(argv, "--model", a.Model)
	}
	return argv
}

// Use names, from [agents], the agent each job runs on: Session, the session a button opens (code investigation and
// implementation); Headless, a brief's runs; Decide, the light headless jobs: a Jira triage, a Jira update's refresh of its
// task and the Jira check of a closed task. An area's empty one is the board's, and the board's is defaultUse's.
type Use struct {
	Session  string `json:"session,omitempty" toml:"session,omitempty"`
	Headless string `json:"headless,omitempty" toml:"headless,omitempty"`
	Decide   string `json:"decide,omitempty" toml:"decide,omitempty"`
}

// defaultAgents and defaultUse are what config.toml's own [agents] and [use] add to and override: Haiku for the light jobs,
// Opus for code.
var (
	defaultAgents = map[string]Agent{"haiku": {Kind: "claude-code", Model: "haiku"}, "opus": {Kind: "claude-code", Model: "opus"}}
	defaultUse    = Use{Session: "opus", Headless: "opus", Decide: "haiku"}
)

// pick is u's agent for job: "session", "headless" or "decide".
func (u Use) pick(job string) string {
	switch job {
	case "session":
		return u.Session
	case "headless":
		return u.Headless
	}
	return u.Decide
}

// agentFor is the agent job runs on in area (an id, "" for none): run's own (a brief run's agent), else the area's use, else
// the board's.
func (b *Board) agentFor(job, area, run string) Agent {
	if i := slices.IndexFunc(b.areas, func(a Area) bool { return a.ID == area }); i >= 0 {
		run = cmp.Or(run, b.areas[i].Use.pick(job))
	}
	name := cmp.Or(run, b.conf.Use.pick(job), defaultUse.pick(job))
	if a, ok := b.conf.Agents[name]; ok {
		return a
	}
	return defaultAgents[name]
}

// badRun says r is neither a prompt nor the roadmap run.
func badRun(r Run) bool { //nolint:gocritic // slices.IndexFunc hands it by value
	return (r.Prompt == "") == (r.Builtin == "") || r.Builtin != "" && r.Builtin != "roadmap"
}

// defaultRuns is an area's brief when config.toml gives it no runs: its roadmap files; runs = [] is no brief.
var defaultRuns = []Run{{Builtin: "roadmap"}}

// defaultRoadmap is an area's roadmap when config.toml gives it none; roadmap = [] reads no files.
var defaultRoadmap = []string{"roadmap.md", "ROADMAP.md", "roadmap.txt", "plan.md", "PLAN.md", "plan.txt", "todo.md", "TODO.md", "todo.txt"}

// roadmapFiles is the files a's roadmap globs match, each once.
func (a *Area) roadmapFiles() []string {
	var files []string
	for _, g := range a.Roadmap {
		matches, _ := filepath.Glob(filepath.Join(a.Dir, g)) // the only error is a bad pattern, which matches nothing
		for _, f := range matches {
			if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() && !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
	}
	return files
}

// parseAreas checks config.toml's areas, each with its folder, and fills in their defaults. A missing name is the id; a dir
// of "~" or "~/..." is at home.
func (b *Board) parseAreas(conf []Area) ([]Area, error) {
	areas := slices.Clone(conf)
	seen := map[string]bool{}
	for i := range areas {
		a := &areas[i]
		if !areaRe.MatchString(a.ID) || seen[a.ID] {
			return nil, fmt.Errorf("parseAreas: id %q: want a unique lowercase folder-like name", a.ID)
		}
		if a.Dir == "" {
			return nil, fmt.Errorf("parseAreas: %s: no dir", a.ID)
		}
		seen[a.ID] = true
		a.Name = cmp.Or(a.Name, a.ID)
		a.IssueURL = cmp.Or(a.IssueURL, os.Getenv("HIRDMAN_ISSUE_URL"))
		if a.Roadmap == nil {
			a.Roadmap = defaultRoadmap
		}
		if a.Runs == nil {
			a.Runs = defaultRuns
		}
		if i := slices.IndexFunc(a.Runs, badRun); i >= 0 {
			return nil, fmt.Errorf("parseAreas: %s: run %d: want a prompt or builtin = \"roadmap\"", a.ID, i+1)
		}
		if every := cmp.Or(a.BriefEvery, os.Getenv("HIRDMAN_BRIEF_EVERY")); every != "" {
			var err error
			if a.every, err = time.ParseDuration(every); err != nil {
				return nil, fmt.Errorf("parseAreas: %s: briefEvery: %w", a.ID, err)
			}
		}
		if a.Dir == "~" || strings.HasPrefix(a.Dir, "~/") {
			a.Dir = filepath.Join(b.home, a.Dir[1:])
		}
		// Its own repository before the board's: HIRDMAN_PR_URL is one repository's, and a #number elsewhere is another's.
		if a.PRURL == "" {
			a.PRURL = cmp.Or(repoPulls(a.Dir), os.Getenv("HIRDMAN_PR_URL"))
		}
	}
	return areas, nil
}

// ghRemoteRe is a GitHub remote's owner/repo: git@github.com:o/r.git, https://github.com/o/r, ssh://git@github.com/o/r.git.
var ghRemoteRe = regexp.MustCompile(`github\.com[:/]([^/\s]+/[^/\s]+?)(?:\.git)?/?$`)

// repoPulls is the pull request link base of dir's origin when that is on GitHub, else "" (no repository, no origin, elsewhere).
func repoPulls(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin") //nolint:gosec // argv, no shell; the area's own folder
	// Without an inherited GIT_DIR (a git hook's), which would answer for that repository instead of dir's.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "GIT_") })
	out, err := cmd.Output()
	if m := ghRemoteRe.FindSubmatch(bytes.TrimSpace(out)); err == nil && m != nil {
		return "https://github.com/" + string(m[1]) + "/pull/"
	}
	return ""
}

// dirFor is the folder a session on t runs in: its area's; an unknown area or a missing folder is an error.
func (b *Board) dirFor(t *Task) (string, error) {
	i := slices.IndexFunc(b.areas, func(a Area) bool { return a.ID == t.Area })
	if i < 0 {
		return "", fmt.Errorf("dirFor: no area %q in config.toml", t.Area)
	}
	if fi, err := os.Stat(b.areas[i].Dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("dirFor: area %s: %s is not a folder", t.Area, b.areas[i].Dir)
	}
	return b.areas[i].Dir, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (b *Board) load() error {
	b.st = State{Brief: map[string]any{}, Tasks: map[string]*Task{}, JiraSeen: map[string][]string{}}
	data, err := os.ReadFile(b.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	if err := json.Unmarshal(data, &b.st); err != nil {
		return fmt.Errorf("load: %w", err)
	}
	// A task's area is required; ones from before that are work. The gone groups load as what replaced them.
	for _, t := range b.st.Tasks {
		t.Area = cmp.Or(t.Area, "work")
		if t.Job != nil {
			t.Job.Question = nil // its hook's request ended with the last run, and the session asks in its terminal
		}
		switch t.Group {
		case "urgent":
			t.Urgency = cmp.Or(t.Urgency, "high")
		case "later":
			t.Urgency = cmp.Or(t.Urgency, "low")
		case "waiting_on_others":
			if t.Status == "todo" || t.Status == "doing" {
				t.Status = "waiting"
			}
		}
		if goneGroups[t.Group] != "" {
			t.Group = ""
		}
		defaultLevels(t)
	}
	return nil
}

// save writes atomically; callers hold mu.
func (b *Board) save() error {
	data, err := json.MarshalIndent(b.st, "", "  ")
	if err != nil {
		return fmt.Errorf("save: %w", err)
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	return nil
}

// saveOrLog is save for the background loops, which have nobody to answer; callers hold mu.
func (b *Board) saveOrLog() {
	if err := b.save(); err != nil {
		log.Print(err)
	}
}

func (b *Board) note(t *Task, actor, kind, text string) {
	e := Entry{At: time.Now(), Actor: actor, Kind: kind, Text: text}
	t.History = append(t.History, e)
	t.UpdatedAt = time.Now()
	b.archive(&LogEntry{Entry: e, Task: t.ID, Title: t.Title, Area: t.Area})
}

// weekOf is the local Monday that starts t's week, as YYYY-MM-DD; it names the week's log file.
func weekOf(t time.Time) string {
	return t.AddDate(0, 0, -(int(t.Weekday())+6)%7).Format(time.DateOnly)
}

// archive appends e to its week's file; callers hold mu.
func (b *Board) archive(e *LogEntry) {
	f, err := os.OpenFile(filepath.Join(b.logDir, weekOf(e.At)+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("archive: %v", err)
		return
	}
	line, err := json.Marshal(e)
	if err == nil {
		_, err = f.Write(append(line, '\n'))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		log.Printf("archive: %v", err)
	}
}

// handleLog returns one week's entries: ?week=YYYY-MM-DD (any day of it), this week by default.
func (b *Board) handleLog(w http.ResponseWriter, r *http.Request) {
	week := weekOf(time.Now())
	if q := r.URL.Query().Get("week"); q != "" {
		d, err := time.Parse(time.DateOnly, q)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "week: want YYYY-MM-DD"})
			return
		}
		week = weekOf(d)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entries := []LogEntry{}
	// week is a formatted date, so the name stays inside logDir.
	if f, err := os.Open(filepath.Join(b.logDir, week+".jsonl")); err == nil { //nolint:gosec // see above
		rd := bufio.NewReader(f)
		for {
			line, err := rd.ReadBytes('\n')
			var e LogEntry
			if json.Unmarshal(line, &e) == nil {
				entries = append(entries, e)
			}
			if err != nil {
				break
			}
		}
		_ = f.Close() // read only: nothing to lose
	}
	older := false
	files, _ := os.ReadDir(b.logDir)
	for _, f := range files {
		older = older || strings.HasSuffix(f.Name(), ".jsonl") && f.Name() < week+".jsonl"
	}
	writeJSON(w, 200, map[string]any{"week": week, "entries": entries, "older": older})
}

func setStatus(t *Task, status string) {
	t.Status = status
	if status == "done" || status == "dropped" {
		t.Shipping = false
		if t.DoneAt == nil {
			now := time.Now()
			t.DoneAt = &now
		}
	} else {
		t.DoneAt = nil
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func (b *Board) handleState(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tasks := make([]*Task, 0, len(b.st.Tasks))
	for _, t := range b.st.Tasks {
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].Order < tasks[j].Order || tasks[i].Order == tasks[j].Order && tasks[i].ID < tasks[j].ID
	})
	writeJSON(w, 200, map[string]any{"brief": b.st.Brief, "tasks": tasks, "log": b.st.Log, "briefs": b.st.Briefs,
		"config": map[string]any{"issue": os.Getenv("HIRDMAN_ISSUE_URL"), "pr": os.Getenv("HIRDMAN_PR_URL"), "areas": b.areas}})
}

func (b *Board) handleBrief(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.Brief = body
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, body)
}

// handleBriefRun briefs the areas the board's button names ({"areas": [ids]}, the ones the filter shows) now;
// an area with nothing to brief, or whose brief is running, is left out.
func (b *Board) handleBriefRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Areas []string `json:"areas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Areas) == 0 {
		writeJSON(w, 400, map[string]string{"error": "areas: required, the ids of the areas to brief"})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for _, id := range body.Areas {
		i := slices.IndexFunc(b.areas, func(a Area) bool { return a.ID == id })
		if i < 0 {
			writeJSON(w, 400, map[string]string{"error": "areas: no area " + id + " in config.toml"})
			return
		}
		if b.briefs(&b.areas[i]) && !b.briefing(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeJSON(w, 409, map[string]string{"error": "nothing to brief: those areas have no brief, or theirs is running"})
		return
	}
	b.briefNow(ids...)
	writeJSON(w, 202, map[string]any{"status": "queued", "areas": ids})
}

// mergePatch returns t with patch's fields over it. jiraStatus merges per key, so updating one
// ticket keeps the others.
func mergePatch(t *Task, patch map[string]json.RawMessage) (Task, error) {
	if raw, ok := patch["jiraStatus"]; ok {
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			return Task{}, fmt.Errorf("jiraStatus: %w", err)
		}
		merged := map[string]string{}
		maps.Copy(merged, t.JiraStatus)
		maps.Copy(merged, m)
		data, err := json.Marshal(merged)
		if err != nil {
			return Task{}, fmt.Errorf("jiraStatus: %w", err)
		}
		patch["jiraStatus"] = data
	}
	var fields map[string]json.RawMessage
	if err := remarshal(t, &fields); err != nil {
		return Task{}, err
	}
	maps.Copy(fields, patch)
	var next Task
	if err := remarshal(fields, &next); err != nil {
		return Task{}, err
	}
	return next, nil
}

// remarshal moves from into to through JSON.
func remarshal(from, to any) error {
	data, err := json.Marshal(from)
	if err == nil {
		err = json.Unmarshal(data, to)
	}
	if err != nil {
		return fmt.Errorf("task: %w", err)
	}
	return nil
}

// defaultLevels sets an unset urgency or importance to medium: every task has both.
func defaultLevels(t *Task) {
	t.Urgency, t.Importance = cmp.Or(t.Urgency, "medium"), cmp.Or(t.Importance, "medium")
}

// goneGroups are the groups a field now says, each with what to set instead.
var goneGroups = map[string]string{"urgent": `urgency "high"`, "later": `urgency "low"`, "waiting_on_others": `status "waiting"`}

// badField says what is wrong with a merged task's area, group or levels, or "".
func (b *Board) badField(t *Task) string {
	levels := []string{"high", "medium", "low"}
	switch {
	case !slices.ContainsFunc(b.areas, func(a Area) bool { return a.ID == t.Area }):
		return "area: required, an id from config.toml's areas"
	case goneGroups[t.Group] != "":
		return fmt.Sprintf("group: %q is gone, set %s", t.Group, goneGroups[t.Group])
	case !slices.Contains(levels, t.Urgency) || !slices.Contains(levels, t.Importance):
		return "urgency, importance: want high, medium or low"
	case slices.ContainsFunc(t.Triggers, func(tr Trigger) bool {
		kinds := 0
		for _, set := range []bool{strings.TrimSpace(tr.On) != "", strings.TrimSpace(tr.JiraStatus) != "", tr.At != nil} {
			if set {
				kinds++
			}
		}
		return kinds != 1 || strings.TrimSpace(tr.Do) == ""
	}):
		return "triggers: each needs do and one of on (a branch), jiraStatus or at"
	}
	return ""
}

// whoSep splits a who that names several people: "Bar + Polina", "Bar/Polina", "Bar & Polina", "Bar and Polina".
var whoSep = regexp.MustCompile(`\s*(?:[,+&/]|\band\b)\s*`)

// fullNames rewrites t's who as full names, comma-separated, each once: a lone first name becomes the one full name
// with that first name on the board (this task included). The caller holds b.mu.
func (b *Board) fullNames(t *Task) {
	full := map[string][]string{} // lowercased first name: the full names that start with it
	for _, o := range append(slices.Collect(maps.Values(b.st.Tasks)), t) {
		for _, n := range whoSep.Split(strings.TrimSpace(o.Who), -1) {
			if first, _, ok := strings.Cut(n, " "); ok && !slices.Contains(full[strings.ToLower(first)], n) {
				full[strings.ToLower(first)] = append(full[strings.ToLower(first)], n)
			}
		}
	}
	var names []string
	for _, n := range whoSep.Split(strings.TrimSpace(t.Who), -1) {
		if f := full[strings.ToLower(n)]; len(f) == 1 {
			n = f[0]
		}
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	t.Who = strings.Join(names, ", ")
}

// handleUpsert merges the body's fields into the task; history, job and files are server-owned.
func (b *Board) handleUpsert(w http.ResponseWriter, r *http.Request) { //nolint:funlen // one step per field rule, in order
	id := r.PathValue("id")
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	delete(patch, "history")
	delete(patch, "job")
	delete(patch, "files")
	delete(patch, "id")
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[id]
	if !ok {
		t = &Task{ID: id, Status: "todo", History: []Entry{}}
	}
	prevStatus := t.Status
	next, err := mergePatch(t, patch)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	defaultLevels(&next)
	b.fullNames(&next)
	linkPulls(&next, next.Summary)
	if bad := b.badField(&next); bad != "" {
		writeJSON(w, 400, map[string]string{"error": bad})
		return
	}
	if !ok && r.URL.Query().Get("actor") == "eugene" {
		b.note(&next, "eugene", "created", "Added on the board.")
	}
	if !ok {
		go b.prep(id)
	}
	if ok && next.Area != t.Area {
		b.note(&next, cmp.Or(r.URL.Query().Get("actor"), "agent"), "area", t.Area+" → "+next.Area)
	}
	// A session's findings often land in the story before its report; the brief's daily rewrites stay out of history.
	if ok && next.Summary != t.Summary && t.Job != nil && t.Job.Status == "running" {
		b.note(&next, cmp.Or(r.URL.Query().Get("actor"), "agent"), "summary", next.Summary)
	}
	if _, set := patch["status"]; set && next.Status != prevStatus {
		setStatus(&next, next.Status)
		if ok {
			// ?actor=eugene marks a drag on the board; the brief's writes leave it unset.
			b.note(&next, cmp.Or(r.URL.Query().Get("actor"), "agent"), "status", prevStatus+" → "+next.Status)
		}
		if next.Status == "done" {
			b.doneNow(&next)
		}
		// Closed on the board; a session closing its own task stays open for Eugene.
		if (next.Status == "done" || next.Status == "dropped") && r.URL.Query().Get("actor") == "eugene" {
			b.stopSession(&next)
		}
	}
	next.UpdatedAt = time.Now()
	b.st.Tasks[id] = &next
	if err = b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, next)
}

func (b *Board) handleNote(w http.ResponseWriter, r *http.Request) {
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil || e.Text == "" {
		writeJSON(w, 400, map[string]string{"error": "need {actor, kind, text}"})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[r.PathValue("id")]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such task"})
		return
	}
	b.note(t, e.Actor, e.Kind, e.Text)
	linkPulls(t, e.Text)
	// A session's report on its action ends the job; the session itself stays open. "claude" is the actor's name before the rename,
	// which skills and sessions started before it still post.
	if j := t.Job; j != nil && j.Status == "running" && j.Session != "" && (e.Actor == "agent" || e.Actor == "claude") && e.Kind == j.Action {
		now := time.Now()
		j.Status, j.EndedAt = "ok", &now
	}
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

// handleDelete removes the task and its attached files for good; the week's log keeps a line saying so.
func (b *Board) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[id]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such task"})
		return
	}
	b.stopSession(t)
	// A session the board can't stop (no pane) would write the task back on its next update.
	if b.busy(t) {
		writeJSON(w, 409, map[string]string{"error": "The agent is working on this task"})
		return
	}
	if filepath.IsLocal(id) {
		if err := os.RemoveAll(filepath.Join(filepath.Dir(b.path), "files", id)); err != nil { //nolint:gosec // filepath.IsLocal(id) above
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	b.note(t, "eugene", "delete", "Deleted from the board.")
	delete(b.st.Tasks, id)
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"deleted": id})
}

// handleCombine folds two tasks into one, {"with": id}: the older stays and takes the newer's history, story,
// references and files (see combine); the newer is removed, its session stopped as handleDelete does.
func (b *Board) handleCombine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		With string `json:"with"`
	}
	id := r.PathValue("id")
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.With == "" || req.With == id {
		writeJSON(w, 400, map[string]string{"error": "need {with}: another task's id"})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	keep, ok1 := b.st.Tasks[id]
	gone, ok2 := b.st.Tasks[req.With]
	if !ok1 || !ok2 {
		writeJSON(w, 404, map[string]string{"error": "no such task"})
		return
	}
	if older(gone, keep) {
		keep, gone = gone, keep
	}
	b.stopSession(gone)
	// A session the board can't stop (no pane) would write the newer task back on its next update.
	if b.busy(gone) {
		writeJSON(w, 409, map[string]string{"error": fmt.Sprintf("The agent is working on “%s”", gone.Title)})
		return
	}
	if err := b.combine(keep, gone); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	delete(b.st.Tasks, gone.ID)
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, keep)
}

// older reports whether a started before c: by their since dates when both have one and they differ, else by
// their first history entries.
func older(a, c *Task) bool {
	if a.Since != "" && c.Since != "" && a.Since != c.Since {
		return a.Since < c.Since
	}
	first := func(t *Task) time.Time {
		if len(t.History) > 0 {
			return t.History[0].At
		}
		return t.UpdatedAt
	}
	return first(a).Before(first(c))
}

// combine moves gone's history, story, references and files into keep; keep's own fields win where both have
// one. A file whose move fails stays gone's, so nothing is lost. Callers hold mu.
func (b *Board) combine(keep, gone *Task) error {
	if err := b.moveFiles(keep, gone); err != nil {
		return err
	}
	keep.History = append(keep.History, gone.History...)
	slices.SortStableFunc(keep.History, func(x, y Entry) int { return x.At.Compare(y.At) })
	if gone.Summary != "" {
		keep.Summary = strings.TrimSpace(keep.Summary + "\n\n**" + gone.Title + "**: " + gone.Summary)
	}
	same := func(x, y string) bool { return x == y }
	keep.Tags = union(keep.Tags, gone.Tags, same)
	keep.Jira = union(keep.Jira, gone.Jira, same)
	keep.Links = union(keep.Links, gone.Links, maps.Equal)
	keep.Messages = union(keep.Messages, gone.Messages, maps.Equal)
	keep.Triggers = append(keep.Triggers, gone.Triggers...)
	for k, v := range gone.JiraStatus {
		if _, ok := keep.JiraStatus[k]; !ok {
			if keep.JiraStatus == nil {
				keep.JiraStatus = map[string]string{}
			}
			keep.JiraStatus[k] = v
		}
	}
	who := union(whoSep.Split(keep.Who, -1), whoSep.Split(gone.Who, -1), same)
	keep.Who = strings.Join(slices.DeleteFunc(who, func(n string) bool { return n == "" }), ", ")
	keep.WaitingFor = cmp.Or(keep.WaitingFor, gone.WaitingFor)
	keep.Since = cmp.Or(keep.Since, gone.Since)
	if keep.Thread == nil {
		keep.Thread = gone.Thread
	}
	keep.Shipping = keep.Shipping || gone.Shipping
	b.note(keep, "eugene", "combine", fmt.Sprintf("Combined with “%s” (%s), which is removed.", gone.Title, gone.ID))
	return nil
}

// moveFiles moves gone's attached files to keep's folder, one at a time. Callers hold mu.
func (b *Board) moveFiles(keep, gone *Task) error {
	files := filepath.Join(filepath.Dir(b.path), "files")
	if len(gone.Files) > 0 && !filepath.IsLocal(keep.ID) {
		return fmt.Errorf("moveFiles: task id %q can't hold files", keep.ID)
	}
	for len(gone.Files) > 0 {
		f := gone.Files[0]
		to := filepath.Join(files, keep.ID, filepath.Base(f))
		if slices.Contains(keep.Files, to) {
			to = filepath.Join(files, keep.ID, gone.ID+"-"+filepath.Base(f))
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return fmt.Errorf("moveFiles: %w", err)
		}
		if err := os.Rename(f, to); err != nil {
			return fmt.Errorf("moveFiles: %w", err)
		}
		keep.Files, gone.Files = append(keep.Files, to), gone.Files[1:]
	}
	if filepath.IsLocal(gone.ID) {
		if err := os.RemoveAll(filepath.Join(files, gone.ID)); err != nil {
			return fmt.Errorf("moveFiles: %w", err)
		}
	}
	return nil
}

// union is a with each of c's items it lacks appended.
func union[T any](a, c []T, eq func(x, y T) bool) []T {
	for _, x := range c {
		if !slices.ContainsFunc(a, func(y T) bool { return eq(x, y) }) {
			a = append(a, x)
		}
	}
	return a
}

// handleAsk is a session's AskUserQuestion hook (see spawnSession): the question shows on the task, and the request
// holds until Eugene answers there (handleAnswer), its reply answering the tool. An empty reply hands the question to
// the session's own prompt in its terminal, as does the hook's request failing: a timeout, a board restart.
func (b *Board) handleAsk(w http.ResponseWriter, r *http.Request) {
	var hook struct {
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil || len(hook.ToolInput) == 0 {
		writeJSON(w, 400, map[string]string{"error": "need a PreToolUse hook's input"})
		return
	}
	id, ch := r.PathValue("id"), make(chan []byte, 1)
	b.mu.Lock()
	t, ok := b.st.Tasks[id]
	if !ok || t.Job == nil || b.asks[id] != nil {
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if b.asks == nil {
		b.asks = map[string]chan []byte{}
	}
	b.asks[id], t.Job.Question = ch, hook.ToolInput
	b.saveOrLog()
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.asks[id] != ch {
			return // answered, and handleAnswer cleared it
		}
		delete(b.asks, id)
		if t, ok := b.st.Tasks[id]; ok && t.Job != nil {
			t.Job.Question = nil
		}
		b.saveOrLog()
	}()
	select {
	case out := <-ch:
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(out); err != nil {
			log.Printf("ask: %v", err)
		}
	case <-r.Context().Done():
	}
}

// handleAnswer answers the question a task's session waits on: {"answers": {question: answer}}, an answer being an
// option's label, several joined by ", " as the agent's own prompt does, or Eugene's own words. No answers hand the
// question back to the session's terminal.
func (b *Board) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Answers map[string]string `json:"answers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := r.PathValue("id")
	t, ok := b.st.Tasks[id]
	ch := b.asks[id]
	if !ok || ch == nil || t.Job == nil {
		writeJSON(w, 409, map[string]string{"error": "no question is waiting on this task"})
		return
	}
	var out []byte
	text := "Answer it in the session's terminal."
	if len(req.Answers) > 0 {
		var input map[string]any
		var asked struct {
			Questions []struct {
				Question string `json:"question"`
			} `json:"questions"`
		}
		if err := json.Unmarshal(t.Job.Question, &input); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		_ = json.Unmarshal(t.Job.Question, &asked) // it parsed as input just above
		input["answers"] = req.Answers
		var err error
		out, err = json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "allow", "updatedInput": input}})
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		lines := make([]string, 0, len(asked.Questions))
		for _, q := range asked.Questions {
			lines = append(lines, q.Question+" → "+req.Answers[q.Question])
		}
		text = strings.Join(lines, "\n")
	}
	ch <- out
	delete(b.asks, id)
	t.Job.Question = nil
	b.note(t, "eugene", "answer", text)
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

const maxAttach = 32 << 20

// handleAttach stores the multipart "file" fields in files/<id>/ next to the state file and lists
// them on the task, so the next session on it reads them. A file with the same name is replaced.
func (b *Board) handleAttach(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAttach)
	if err := r.ParseMultipartForm(maxAttach); err != nil { //nolint:gosec // the body is capped by MaxBytesReader above
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	uploads := r.MultipartForm.File["file"]
	id := r.PathValue("id")
	// The id and the file names become paths: keep each a single plain name.
	if len(uploads) == 0 || !filepath.IsLocal(id) || slices.ContainsFunc(uploads, func(fh *multipart.FileHeader) bool {
		name := filepath.Base(fh.Filename)
		return name == "." || !filepath.IsLocal(name)
	}) {
		writeJSON(w, 400, map[string]string{"error": "need one or more files, each with a plain name"})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[id]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such task"})
		return
	}
	dir := filepath.Join(filepath.Dir(b.path), "files", id)
	var names []string
	for _, fh := range uploads {
		name := filepath.Base(fh.Filename)
		path := filepath.Join(dir, name)
		if err := saveUpload(fh, path); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if !slices.Contains(t.Files, path) {
			t.Files = append(t.Files, path)
		}
		names = append(names, name)
	}
	b.note(t, "eugene", "attach", strings.Join(names, ", "))
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

func saveUpload(fh *multipart.FileHeader, path string) error {
	src, err := fh.Open()
	if err != nil {
		return fmt.Errorf("saveUpload: %w", err)
	}
	defer func() { _ = src.Close() }()
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // path is checked by handleAttach
		return fmt.Errorf("saveUpload: %w", err)
	}
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // path is checked by handleAttach
	if err != nil {
		return fmt.Errorf("saveUpload: %w", err)
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("saveUpload: %w", err)
	}
	return nil
}

type actRequest struct {
	Action  string `json:"action"` // reply | act | qa | drop; update comes from the Jira watcher
	Comment string `json:"comment"`
	Key     string `json:"key"`   // qa only: the Jira key to move
	Fresh   bool   `json:"fresh"` // a new agent session, not the task's last one resumed or continued
}

func (b *Board) handleAct(w http.ResponseWriter, r *http.Request) {
	var req actRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[r.PathValue("id")]
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such task"})
		return
	}
	switch req.Action {
	case "drop":
		setStatus(t, "dropped")
		b.stopSession(t)
		b.note(t, "eugene", "drop", strings.TrimSpace("Dropped. "+req.Comment))
	case "reply", "act", "qa":
		if req.Action == "qa" && !slices.Contains(t.Jira, req.Key) {
			writeJSON(w, 400, map[string]string{"error": "qa needs one of this task's Jira keys"})
			return
		}
		if b.busy(t) {
			if code, err := b.addGuidance(t, req); err != nil {
				writeJSON(w, code, map[string]string{"error": err.Error()})
				return
			}
			break
		}
		text := req.Comment
		if req.Action == "qa" {
			text = strings.TrimSpace("Move " + req.Key + " to QA - Staging. " + req.Comment)
		}
		if text == "" {
			text = "(no guidance)"
		}
		b.note(t, "eugene", req.Action, text)
		b.start(t, req)
	default:
		writeJSON(w, 400, map[string]string{"error": "action must be reply, act, qa or drop"})
		return
	}
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, t)
}

// actRule is what act asks; a trigger's own instruction is done the same way.
const actRule = `Do the task itself, and what Eugene's guidance asks, as far as it can be done safely now. If that is getting
missing information, investigate (Jira, chat, code, public API) and answer it yourself; if only a person can answer it,
ask them in the thread where the item came from and send it. Follow the CLAUDE.md, skills and memory of the folder you
run in, which the task's area picks (in the be repo: branch from development, pre-PR checklist, PR review post).
Never merge, push to main, deploy or touch prod data. A task with source "roadmap" came from the roadmap file its
summary names: once the work is done, mark its item done there, the way that file marks done items, in the work's
own commit when git tracks the file, or just edited when it doesn't.`

// actionRules says what each action means; a session is sent only its own.
var actionRules = map[string]string{
	"reply": `Write the reply to the person in the task and SEND it now, in the room and thread where they raised it
(task.thread names the room, reply_to and thread when known; otherwise find the message with kith-mcp).
Write in that room's language and in Eugene's voice. Send once: never retry inside the room's cooldown,
and report exactly what kith answered, "sent" or "drafted". A Jira question gets a Jira comment instead.
An email (task.thread.email is its Gmail thread id) gets a Gmail draft reply in that thread, never a sent one.`,
	"act": actRule,
	"qa": `The task is done but its Jira ticket is still before QA. Transition that one key to "QA - Staging"
(read its transitions first). A sub-task that has no QA state goes straight to Done. Change nothing else, then
record the new status: PUT {"jiraStatus": {"<KEY>": "<status now>"}}.`,
	"update": `The update came from the Jira feed, not from Eugene. Read the ticket in Jira (status, newest
comments) and refresh this task: summary (what changed, who said what), jiraStatus, messages. If the update asks
something of Eugene (a mention, a question, an assignment), set group "waiting_on_you" and status "todo".
Never send messages and never change Jira on an update.`,
	"trigger": `The trigger (one of task.triggers) fired: a pull request the task links reached its branch, one of the
task's Jira keys reached its status, or its time came. Do what the trigger says: it is Eugene's own instruction,
given ahead. ` + actRule,
}

func (b *Board) prompt(t *Task, req actRequest) (string, error) {
	view := *t
	view.Job = nil
	taskJSON, err := json.Marshal(view)
	if err != nil {
		return "", fmt.Errorf("prompt: %w", err)
	}
	guidance := req.Comment
	if guidance == "" {
		guidance = "none"
	}
	var sb strings.Builder
	// A press into the task's session sends only its message: the session has the task and the board's ways already.
	if t.Job != nil && t.Job.SessionID != "" && req.Action != "update" {
		if req.Action == "trigger" {
			fmt.Fprintf(&sb, "A trigger Eugene set on this task fired: %s\n", guidance)
		} else {
			fmt.Fprintf(&sb, "Eugene pressed %q on this task again. Eugene's guidance: %s\n", req.Action, guidance)
		}
		if req.Action == "qa" {
			fmt.Fprintf(&sb, "Jira key to move: %s\n", req.Key)
		}
		if len(t.Files) > 0 {
			fmt.Fprintf(&sb, "The task's attached files: %s\n", strings.Join(t.Files, ", "))
		}
		if !slices.Contains(t.Job.Rules, req.Action) {
			fmt.Fprintf(&sb, "\nWhat to do: %s\n\n", actionRules[req.Action])
		}
		fmt.Fprintf(&sb, "When it is done, post your report as before, with kind %q.\n", req.Action)
		return sb.String(), nil
	}
	switch req.Action {
	case "update":
		fmt.Fprintf(&sb, "A Jira update arrived for an item of Eugene's Hirdman board (%s). Nobody is watching this run.\n\nTask:\n%s\n\nThe update: %s\n", b.selfURL, taskJSON, guidance)
	case "trigger":
		fmt.Fprintf(&sb, "A trigger Eugene set on an item of his Hirdman board (%s) fired.\n\nTask:\n%s\n\nThe trigger: %s\n", b.selfURL, taskJSON, guidance)
	default:
		fmt.Fprintf(&sb, "Eugene pressed %q on an item of his Hirdman board (%s).\n\nTask:\n%s\n\nEugene's guidance: %s\n", req.Action, b.selfURL, taskJSON, guidance)
	}
	if req.Action == "qa" {
		fmt.Fprintf(&sb, "Jira key to move: %s\n", req.Key)
	}
	if len(t.Files) > 0 {
		sb.WriteString("Eugene attached files to this task (task.files): read them, they are part of it.\n")
	}
	fmt.Fprintf(&sb, "\nWhat to do: %s\n\nKeep the board current while you work (JSON body, fields merge):\n"+
		"  curl -s -X PUT %[2]s/api/tasks/%[3]s -H 'Content-Type: application/json' -d '{\"status\":\"waiting\",\"waitingFor\":\"PR review\"}'   # todo|doing|waiting|done|dropped; waitingFor: what who owes; start it with \"PR review\" when that is all, and the merge closes the task\n"+
		"  curl -s -X PUT %[2]s/api/tasks/%[3]s -H 'Content-Type: application/json' -d '{\"triggers\":[{\"on\":\"main\",\"do\":\"...\"}]}'   # work left for after the merge: a session starts on the task to do it once a pull request it links reaches that branch (or \"at\": an RFC 3339 time, or \"jiraStatus\"); this replaces task.triggers, so keep the ones it has\n"+
		"  curl -s -X PUT %[2]s/api/tasks/%[3]s -H 'Content-Type: application/json' -d '{\"summary\":\"...\"}'    # when the story changed\n",
		actionRules[req.Action], b.selfURL, t.ID)
	if req.Action == "update" {
		sb.WriteString("Your final message goes into the task's history as is: 2-4 plain lines saying what you did, what was sent or drafted, and links.\n")
	} else {
		fmt.Fprintf(&sb, "This is an interactive session Eugene may attach to and steer; the board does not see your final message. "+
			"When the action is done, post your report once, 2-4 plain lines saying what you did, what was sent or drafted, and links; "+
			"it goes into the task's history and marks the job finished:\n"+
			"  jq -n --arg text '...' '{actor: \"agent\", kind: \"%[3]s\", text: $text}' | curl -s -X POST %[1]s/api/tasks/%[2]s/notes -H 'Content-Type: application/json' -d @-\n"+
			"Then stay open for Eugene.\n", b.selfURL, t.ID, req.Action)
	}
	return sb.String(), nil
}

// start runs the agent on t; callers hold mu. A Jira update runs headless; a button press gets an
// interactive session in the wezterm mux server, which outlives a board restart and can be attached to.
// The task keeps one agent conversation: a press goes into its open session, or resumes it once closed, unless req.Fresh.
func (b *Board) start(t *Task, req actRequest) {
	if req.Fresh {
		b.stopSession(t)
	}
	prev := t.Job
	t.Job = &Job{Action: req.Action, Status: "running", StartedAt: time.Now()}
	if prev != nil && !req.Fresh {
		t.Job.SessionID, t.Job.Rules = prev.SessionID, prev.Rules
	}
	prompt, err := b.prompt(t, req)
	if err == nil && req.Action == "update" {
		var dir string
		if dir, err = b.dirFor(t); err == nil {
			go b.run(b.agentFor("decide", t.Area, ""), t.ID, dir, req.Action, prompt, prev, b.mcpFor(t.Area))
			return
		}
	}
	verb := "Started"
	switch {
	case err != nil:
	case !req.Fresh && sessionPane(prev):
		verb, t.Job.Session, t.Job.Pane = "Continued", prev.Session, prev.Pane
		err = sendText(t.Job.Pane, prompt)
	default:
		idFlag := "--resume"
		if t.Job.SessionID != "" {
			verb = "Resumed"
		} else {
			idFlag, t.Job.SessionID = "--session-id", newSessionID()
		}
		t.Job.Session = t.ID + "-" + req.Action
		var dir string
		if dir, err = b.dirFor(t); err == nil {
			t.Job.Pane, err = b.spawnSession(b.agentFor("session", t.Area, ""), dir, t.ID, t.Job.Session,
				slices.Concat(b.mcpFor(t.Area), []string{idFlag, t.Job.SessionID, prompt})...)
		}
	}
	if err != nil {
		now := time.Now()
		t.Job.Status, t.Job.EndedAt = "failed", &now
		b.note(t, "agent", req.Action, "Could not start a session: "+err.Error())
		return
	}
	if !slices.Contains(t.Job.Rules, req.Action) {
		t.Job.Rules = append(slices.Clone(t.Job.Rules), req.Action)
	}
	where := "your terminal"
	if t.Job.Pane != "" {
		where = "wezterm pane " + t.Job.Pane
	}
	b.note(t, "agent", "session", fmt.Sprintf("%s session %q in %s: attach there, or on claude.ai/code (remote control).", verb, t.Job.Session, where))
}

// newSessionID is a random (v4) UUID, the form the agent's --session-id takes.
func newSessionID() string {
	var u [16]byte
	_, _ = rand.Read(u[:]) // crypto/rand never fails
	u[6], u[8] = u[6]&0x0f|0x40, u[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:])
}

// spawnSession opens an interactive session of agent ag and returns its wezterm pane id ("" in another terminal);
// args end the agent's argv: its MCP flags, the session id to start or resume, and the prompt.
// In wezterm the pane closes when the agent exits, so a live pane means a live session.
func (b *Board) spawnSession(ag Agent, dir, id, name string, args ...string) (string, error) {
	// Its questions wait on the board (handleAsk) for up to a day; the task id is path-escaped, so it can't break the quotes.
	post := func(path string) string {
		return fmt.Sprintf("curl -s -X POST '%s/api/tasks/%s/%s' -H 'Content-Type: application/json' --data-binary @-", b.selfURL, url.PathEscape(id), path)
	}
	onSession := []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": post("session")}}}}
	settings, err := json.Marshal(map[string]any{"hooks": map[string]any{
		"PreToolUse": []any{map[string]any{
			"matcher": "AskUserQuestion", "hooks": []any{map[string]any{"type": "command", "command": post("ask"), "timeout": 86400}}}},
		"Stop": onSession, "StopFailure": onSession, "SessionEnd": onSession}})
	if err != nil {
		return "", fmt.Errorf("spawn: %w", err)
	}
	// zsh -lic gives the session the user's login env; the service's own PATH is bare.
	// The agent command and the prompt reach zsh as "$0" and "$@", never as shell text.
	session := slices.Concat([]string{"zsh", "-lic", `exec "$0" "$@"`},
		ag.argv("--permission-mode", "auto", "--remote-control", name, "-n", name, "--settings", string(settings)), args)
	if len(b.terminal) > 0 {
		// shortcut: no pane to watch, so a session that dies without reporting keeps its task busy,
		// and a terminal that stays our child dies with a board restart; track its process if another terminal gets real use.
		cmd := exec.CommandContext(context.Background(), b.terminal[0], slices.Concat(b.terminal[1:], session)...) //nolint:gosec // HIRDMAN_TERMINAL is the service's own setting
		cmd.Dir = dir
		if err = cmd.Start(); err != nil {
			return "", fmt.Errorf("spawn: %w", err)
		}
		go func() { _ = cmd.Wait() }() // reap it; its exit says nothing about the session
		return "", nil
	}
	spawn := []string{"cli", "spawn", "--cwd", dir}
	if b.window {
		spawn = append(spawn, "--new-window")
	}
	pane, err := wezterm(slices.Concat(spawn, []string{"--"}, session))
	if err != nil && !b.window {
		// No GUI attached to take the tab (wezterm cli picks the last-focused client's window): open a mux window.
		pane, err = wezterm(slices.Concat(spawn, []string{"--new-window", "--"}, session))
	}
	return pane, err
}

// wezterm runs wezterm with args and returns its trimmed output.
func wezterm(args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wezterm", args...).Output() //nolint:gosec // args are the session's, passed as argv
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("spawn: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("spawn: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// panes lists the wezterm mux's panes: id -> title.
func panes() (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wezterm", "cli", "list", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("panes: %w", err)
	}
	var list []struct {
		PaneID int    `json:"pane_id"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("panes: %w", err)
	}
	all := make(map[string]string, len(list))
	for _, p := range list {
		all[fmt.Sprint(p.PaneID)] = p.Title
	}
	return all, nil
}

// paneAlive reports whether the wezterm mux still has pane; if the mux can't be asked, it assumes yes.
func paneAlive(pane string) bool {
	all, err := panes()
	if err != nil {
		return true
	}
	_, ok := all[pane]
	return ok
}

// sessionPane reports whether j's session is alive in its pane, which wezterm titles with the session's name:
// pane ids start over with the mux, so a bare id could be someone else's pane by now.
func sessionPane(j *Job) bool {
	if j == nil || j.Pane == "" {
		return false
	}
	all, err := panes()
	return err == nil && strings.Contains(all[j.Pane], j.Session)
}

// stopSession kills t's session pane, ending the agent in it, once Eugene closes, drops or deletes the task. Callers hold mu.
func (b *Board) stopSession(t *Task) {
	j := t.Job
	if !sessionPane(j) {
		return
	}
	if _, err := wezterm([]string{"cli", "kill-pane", "--pane-id", j.Pane}); err != nil {
		b.note(t, "agent", "session", fmt.Sprintf("Could not stop session %q: %v", j.Session, err))
		return
	}
	if j.Status == "running" {
		now := time.Now()
		j.Status, j.EndedAt = "stopped", &now
	}
	b.note(t, "agent", "session", fmt.Sprintf("Stopped session %q.", j.Session))
}

// addGuidance types Eugene's guidance into t's session while the agent works on it: Act then adds to the round,
// not starts one; other actions wait. It returns the HTTP status for its error. Callers hold mu.
func (b *Board) addGuidance(t *Task, req actRequest) (int, error) {
	text := req.Comment
	if req.Action != "act" || !sessionPane(t.Job) {
		return 409, errors.New("busy: the agent is already working on this task")
	}
	if strings.TrimSpace(text) == "" {
		return 400, errors.New("write the guidance to add to the agent's session")
	}
	if err := sendText(t.Job.Pane, text); err != nil {
		return 500, err
	}
	b.note(t, "eugene", "message", text)
	return 200, nil
}

// sendText types text into pane and submits it.
func sendText(pane, text string) error {
	_, err := wezterm([]string{"cli", "send-text", "--pane-id", pane, "--", text})
	if err == nil {
		_, err = wezterm([]string{"cli", "send-text", "--pane-id", pane, "--no-paste", "\r"})
	}
	return err
}

// maxRetries caps how often the board resumes or continues one job's session on its own.
const maxRetries = 3

// retryDelay is the wait before continuing a session after its first passing API error; it doubles with each retry.
var retryDelay = time.Minute

// passing are the StopFailure errors a retry can fix; the others (authentication_failed, billing_error,
// invalid_request, ...) need Eugene.
var passing = map[string]bool{"rate_limit": true, "overloaded": true, "server_error": true, "unknown": true}

const resumePrompt = "The Hirdman board resumed this session: its terminal closed before you reported back (a restart or a crash). " +
	"Check what was already done (messages sent, commits, pushes, board updates) and carry on from there without repeating any of it; " +
	"then report as asked."

// busy reports whether t has a job still running; a session whose pane is gone ended without reporting,
// and is resumed, or else marked failed so the buttons work again. Callers hold mu.
func (b *Board) busy(t *Task) bool {
	if t.Job == nil || t.Job.Status != "running" {
		return false
	}
	if t.Job.Pane == "" || paneAlive(t.Job.Pane) || b.resume(t) {
		return true
	}
	now := time.Now()
	t.Job.Status, t.Job.EndedAt = "failed", &now
	b.note(t, "agent", t.Job.Action, fmt.Sprintf("Session %q ended without reporting back.", t.Job.Session))
	return false
}

// resume reopens t's session in a new pane, up to maxRetries times a job; a session started before the board knew
// session ids can't be. Callers hold mu.
func (b *Board) resume(t *Task) bool {
	j := t.Job
	if j.SessionID == "" || j.Retries >= maxRetries {
		return false
	}
	dir, err := b.dirFor(t)
	pane := ""
	if err == nil {
		pane, err = b.spawnSession(b.agentFor("session", t.Area, ""), dir, t.ID, j.Session,
			slices.Concat(b.mcpFor(t.Area), []string{"--resume", j.SessionID, resumePrompt})...)
	}
	if err != nil {
		b.note(t, "agent", "session", fmt.Sprintf("Could not resume session %q: %v", j.Session, err))
		return false
	}
	j.Pane = pane
	j.Retries++
	b.note(t, "agent", "session", fmt.Sprintf("Session %q closed before reporting back; resumed it in wezterm pane %s.", j.Session, pane))
	return true
}

// handleSessionHook takes a session's Stop, StopFailure and SessionEnd hooks (see spawnSession). A turn that ends before
// the report goes into the history as the agent's message, as the board sees nothing else of its terminal. A turn stopped
// by a passing API error is continued after a backoff, up to maxRetries times a job; any other is noted for Eugene.
// Eugene exiting the session ends its job, so it isn't resumed.
func (b *Board) handleSessionHook(w http.ResponseWriter, r *http.Request) {
	var hook struct {
		Event  string `json:"hook_event_name"`
		Reason string `json:"reason"`
		Error  string `json:"error"`
		Last   string `json:"last_assistant_message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := r.PathValue("id")
	t, ok := b.st.Tasks[id]
	if !ok || t.Job == nil || t.Job.Status != "running" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	j := t.Job
	switch {
	case hook.Event == "Stop" && hook.Last != "":
		b.note(t, "agent", "message", hook.Last)
	case hook.Event == "SessionEnd" && (hook.Reason == "prompt_input_exit" || hook.Reason == "logout"):
		now := time.Now()
		j.Status, j.EndedAt = "stopped", &now
		b.note(t, "agent", "session", fmt.Sprintf("Session %q was exited before reporting back.", j.Session))
	case hook.Event == "StopFailure" && passing[hook.Error] && j.Retries < maxRetries:
		// shortcut: a usage limit that resets hours away uses up the retries in minutes; read its reset time if that bites.
		// shortcut: a board restart drops a pending continue; save it on the job if that bites.
		wait := retryDelay << j.Retries
		j.Retries++
		b.note(t, "agent", "session", fmt.Sprintf("Session %q stopped: %s. Continuing it in %s.", j.Session, cmp.Or(hook.Last, hook.Error), wait))
		time.AfterFunc(wait, func() { b.continueSession(id, j) })
	case hook.Event == "StopFailure":
		b.note(t, "agent", "session", fmt.Sprintf("Session %q stopped: %s. It waits for you in its terminal.", j.Session, cmp.Or(hook.Last, hook.Error)))
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := b.save(); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// continueSession types "continue" into j's session, if j is still id's running job and its session is live.
func (b *Board) continueSession(id string, j *Job) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.st.Tasks[id]; !ok || t.Job != j || j.Status != "running" || !sessionPane(j) {
		return
	}
	text := fmt.Sprintf("Continued session %q.", j.Session)
	if err := sendText(j.Pane, "continue"); err != nil {
		text = fmt.Sprintf("Could not continue session %q: %v", j.Session, err)
	}
	b.note(b.st.Tasks[id], "agent", "session", text)
	b.saveOrLog()
}

// reapSessions resumes or fails every session job whose pane is gone, so the board corrects itself without waiting
// for a button press on that task, and forgets the pane of an ended job whose session has closed since.
func (b *Board) reapSessions() {
	all, err := panes()
	b.mu.Lock()
	defer b.mu.Unlock()
	reaped := false
	for _, t := range b.st.Tasks {
		j := t.Job
		if j != nil && j.Status == "running" && j.Pane != "" {
			pane := j.Pane
			reaped = !b.busy(t) || j.Pane != pane || reaped
		}
		// A session stays open after its report; once its pane is gone the task has no session to send to.
		if err == nil && j != nil && j.Status != "running" && j.Pane != "" && !strings.Contains(all[j.Pane], j.Session) {
			j.Pane = ""
			reaped = true
		}
	}
	if reaped {
		b.saveOrLog()
	}
}

func (b *Board) logf(kind, format string, args ...any) {
	e := Entry{At: time.Now(), Actor: "agent", Kind: kind, Text: fmt.Sprintf(format, args...)}
	b.st.Log = append(b.st.Log, e)
	b.archive(&LogEntry{Entry: e})
	if len(b.st.Log) > logCap {
		b.st.Log = b.st.Log[len(b.st.Log)-logCap:]
	}
}

// run is a Jira update's headless run of agent ag in the task's folder, with the agent's flags; prev is the task's job
// before it, whose session the task keeps for its next Act.
func (b *Board) run(ag Agent, id, dir, action, prompt string, prev *Job, flags []string) {
	status, text := runAgent(ag, dir, prompt, flags...)

	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.st.Tasks[id]
	if !ok {
		return
	}
	now := time.Now()
	t.Job = &Job{Action: action, Status: status, StartedAt: t.Job.StartedAt, EndedAt: &now}
	if prev != nil {
		t.Job.Session, t.Job.Pane, t.Job.SessionID, t.Job.Rules = prev.Session, prev.Pane, prev.SessionID, prev.Rules
	}
	b.note(t, "agent", action, text)
	if err := b.save(); err != nil {
		log.Printf("save after %s on %s: %v", action, id, err)
	}
}

type streamEvent struct {
	Type    string `json:"type"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// runAgent runs one headless session of agent ag, with flags after its own, and returns its final message.
// It reads stream-json because `--output-format json` only prints at exit, and the
// process can linger after its result (async hooks); it's killed 30s after the result.
func runAgent(ag Agent, dir, prompt string, flags ...string) (status, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	argv := ag.argv(append([]string{"-p", prompt, "--permission-mode", "auto", "--output-format", "stream-json", "--verbose"}, flags...)...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the agent is config.toml's, the service's own setting
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HIRDMAN=1")
	// Own process group, so a timeout also kills its hooks and MCP servers.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return "failed", err.Error()
	}

	var res *streamEvent
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var ev streamEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "result" {
			res = &ev
			time.AfterFunc(30*time.Second, cancel)
			break
		}
	}
	werr := cmd.Wait()

	if res != nil {
		if res.IsError {
			return "failed", res.Result
		}
		return "ok", res.Result
	}
	msg := "no result from the agent"
	if werr != nil {
		msg += ": " + werr.Error()
	}
	if e := strings.TrimSpace(stderr.String()); e != "" {
		msg += "\n" + e
	}
	return "failed", msg
}

// failInterrupted marks the headless jobs a restart killed as failed; don't leave them looking alive.
// Sessions live on in their terminal.
func (b *Board) failInterrupted() {
	for _, t := range b.st.Tasks {
		if t.Job != nil && t.Job.Status == "running" && t.Job.Session == "" {
			t.Job.Status = "failed"
			b.note(t, "agent", t.Job.Action, "Interrupted: the board restarted while this was running.")
		}
	}
	for _, j := range b.st.Briefs {
		if j.Status == "running" {
			j.Status = "failed"
		}
	}
}

func (b *Board) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := w.Write(indexHTML); err != nil {
			log.Printf("index: %v", err)
		}
	})
	mux.HandleFunc("GET /api/state", b.handleState)
	mux.HandleFunc("GET /api/log", b.handleLog)
	mux.HandleFunc("PUT /api/brief", b.handleBrief)
	mux.HandleFunc("POST /api/brief/run", b.handleBriefRun)
	mux.HandleFunc("PUT /api/tasks/{id}", b.handleUpsert)
	mux.HandleFunc("DELETE /api/tasks/{id}", b.handleDelete)
	mux.HandleFunc("POST /api/tasks/{id}/combine", b.handleCombine)
	mux.HandleFunc("POST /api/tasks/{id}/notes", b.handleNote)
	mux.HandleFunc("POST /api/tasks/{id}/files", b.handleAttach)
	mux.HandleFunc("POST /api/tasks/{id}/act", b.handleAct)
	mux.HandleFunc("POST /api/tasks/{id}/ask", b.handleAsk)
	mux.HandleFunc("POST /api/tasks/{id}/answer", b.handleAnswer)
	mux.HandleFunc("POST /api/tasks/{id}/session", b.handleSessionHook)
	mux.HandleFunc("GET /api/settings", b.handleSettings)
	mux.HandleFunc("PUT /api/settings", b.handleSaveSettings)
	return mux
}

func main() { //nolint:funlen // one setup step after another
	home, _ := os.UserHomeDir()
	confDir, _ := os.UserConfigDir()
	addr := env("HIRDMAN_ADDR", ":8787")
	path := env("HIRDMAN_STATE", filepath.Join(home, ".local", "state", "hirdman", "state.json"))
	confPath := env("HIRDMAN_CONFIG", filepath.Join(confDir, "hirdman", "config.toml"))
	conf, err := loadConfig(confPath)
	if err != nil {
		log.Fatal(err)
	}
	base := applyConfig(&conf)
	b := &Board{
		path:     path,
		home:     home,
		terminal: strings.Fields(os.Getenv("HIRDMAN_TERMINAL")),
		window:   os.Getenv("HIRDMAN_SESSION_IN") == "window",
		clean:    os.Getenv("HIRDMAN_CLEAN_MERGED") == "1",
		selfURL:  env("HIRDMAN_URL", "http://localhost"+addr),
		triage:   make(chan struct{}, 1),
		wake:     make(chan struct{}, 1),
		conf:     conf,
		confPath: confPath,
		env:      base,
		restart:  make(chan struct{}, 1),
	}
	if b.areas, err = b.checkConfig(&conf); err != nil {
		log.Fatalf("%s: %v", confPath, err)
	}
	if len(b.areas) == 0 {
		log.Printf("%s has no areas yet: add them on the settings page", confPath)
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		log.Fatal(err)
	}
	if err := b.load(); err != nil {
		log.Fatalf("load %s: %v", b.path, err)
	}
	b.logDir = filepath.Join(filepath.Dir(b.path), "log")
	if err := b.seedLog(); err != nil {
		log.Fatal(err)
	}
	b.failInterrupted()
	b.saveOrLog()
	go func() {
		for ; ; time.Sleep(time.Minute) {
			b.reapSessions()
			b.fireTriggers(false) // time and Jira status triggers; watchGitHub's polls fire branch ones
		}
	}()

	// HIRDMAN_ISSUE_KEY: the key pattern in the issue feeds (each area's feeds, unless its own keys) and GitHub notifications.
	keyRe := regexp.MustCompile(env("HIRDMAN_ISSUE_KEY", `\b[A-Z][A-Z0-9]+-\d+\b`))
	for i := range b.areas {
		a := &b.areas[i]
		for _, f := range a.Feeds {
			re, every := keyRe, cmp.Or(envDuration("HIRDMAN_JIRA_POLL"), 2*time.Minute)
			if f.Keys != "" {
				re = regexp.MustCompile(f.Keys) // checkConfig compiled it
			}
			go b.watchFeed(b.conf.MCP[f.MCP], a, f, re, every)
			log.Printf("watching %s for %s's issue updates every %s", f.id(), a.ID, every)
		}
	}
	// HIRDMAN_GITHUB_POLL: how often to read GitHub notifications (via gh) onto the tasks they are about; unset is off.
	go b.watchGitHub(keyRe, envDuration("HIRDMAN_GITHUB_POLL"))
	go b.scheduleBrief()
	for i := range b.areas {
		if a := &b.areas[i]; a.every > 0 && b.briefs(a) {
			log.Printf("briefing %s every %s", a.ID, a.every)
		}
	}

	b.serve(addr)
}

// serve answers until a saved setting asks for a restart; then the board runs itself again, the same process, with the
// service's env and the new config.toml.
func (b *Board) serve(addr string) {
	log.Printf("hirdman on %s, state %s, config %s", addr, b.path, b.confPath)
	srv := &http.Server{Addr: addr, Handler: b.routes(), ReadHeaderTimeout: 10 * time.Second}
	stopped := make(chan struct{})
	go func() {
		<-b.restart
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil { // lets the save's answer go out first
			log.Print(err)
		}
		close(stopped)
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-stopped
	b.mu.Lock() // no state write cut in half
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	log.Print("settings saved, restarting")
	envList := make([]string, 0, len(b.env))
	for k, v := range b.env {
		envList = append(envList, k+"="+v)
	}
	log.Fatal(syscall.Exec(exe, os.Args, envList)) //nolint:gosec // the board's own binary and argv
}

// Config is config.toml: the board's areas and its settings. Each setting has an env var, HIRDMAN_..., which wins over
// the file when set; see settings.
type Config struct {
	BriefEvery  string               `toml:"briefEvery,omitempty"`
	JiraPoll    string               `toml:"jiraPoll,omitempty"`
	GitHubPoll  string               `toml:"githubPoll,omitempty"`
	IssueKey    string               `toml:"issueKey,omitempty"`
	Terminal    string               `toml:"terminal,omitempty"`
	SessionIn   string               `toml:"sessionIn,omitempty"`
	CleanMerged string               `toml:"cleanMerged,omitempty"`
	IssueURL    string               `toml:"issueUrl,omitempty"`
	PRURL       string               `toml:"prUrl,omitempty"`
	URL         string               `toml:"url,omitempty"`
	MCP         map[string]MCPServer `toml:"mcp,omitempty"`
	Agents      map[string]Agent     `toml:"agents,omitempty"`
	Use         Use                  `toml:"use,omitempty"`
	Areas       []Area               `toml:"areas"`
}

// settings is c's settings by their env var: the settings page's keys, and what the rest of the board reads.
func (c *Config) settings() map[string]*string {
	return map[string]*string{"HIRDMAN_BRIEF_EVERY": &c.BriefEvery, "HIRDMAN_JIRA_POLL": &c.JiraPoll, "HIRDMAN_GITHUB_POLL": &c.GitHubPoll,
		"HIRDMAN_ISSUE_KEY": &c.IssueKey, "HIRDMAN_TERMINAL": &c.Terminal,
		"HIRDMAN_SESSION_IN": &c.SessionIn, "HIRDMAN_CLEAN_MERGED": &c.CleanMerged, "HIRDMAN_ISSUE_URL": &c.IssueURL,
		"HIRDMAN_PR_URL": &c.PRURL, "HIRDMAN_URL": &c.URL}
}

// loadConfig reads config.toml; no file is an empty config, which the settings page fills. An unknown key is refused,
// as a typo would otherwise read as unset.
func loadConfig(path string) (Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Config{}, nil
	case err != nil:
		return c, fmt.Errorf("loadConfig: %w", err)
	case len(md.Undecoded()) > 0:
		return c, fmt.Errorf("loadConfig: %s: unknown key %s", path, md.Undecoded()[0])
	}
	return c, nil
}

// applyConfig sets the env from c's settings the env doesn't set itself, before anything reads it, and returns the env from
// before.
func applyConfig(c *Config) (before map[string]string) {
	before = map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		before[k] = v
	}
	for k, v := range c.settings() {
		if before[k] == "" && *v != "" {
			if err := os.Setenv(k, *v); err != nil {
				log.Printf("applyConfig: %v", err)
			}
		}
	}
	return before
}

// handleSettings answers every setting as the board runs with it, and as the service's env has it; the areas as
// config.toml has them.
func (b *Board) handleSettings(w http.ResponseWriter, r *http.Request) {
	areas, err := json.Marshal(b.conf.Areas)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("handleSettings: %v", err)})
		return
	}
	now := map[string]string{"HIRDMAN_STATE": b.path, "HIRDMAN_CONFIG": b.confPath, "HIRDMAN_AREAS": string(areas)}
	env := map[string]string{"HIRDMAN_STATE": b.env["HIRDMAN_STATE"], "HIRDMAN_CONFIG": b.env["HIRDMAN_CONFIG"]}
	for k := range b.conf.settings() {
		now[k], env[k] = os.Getenv(k), b.env[k]
	}
	now["HIRDMAN_ADDR"], env["HIRDMAN_ADDR"] = os.Getenv("HIRDMAN_ADDR"), b.env["HIRDMAN_ADDR"]
	writeJSON(w, 200, map[string]any{"settings": now, "env": env})
}

// handleSaveSettings writes the settings and areas sent ({"HIRDMAN_...": value, "HIRDMAN_AREAS": a JSON list}) over
// config.toml's, then restarts the board. A value the board would refuse at start is refused here, or the restart would
// loop on it; so is a save while the agent runs headless, which the restart would cut off.
// shortcut: the file is written anew: its comments are lost and multi-line prompts come out as one escaped line; write it
// back with its own formatting once the page edits runs.
func (b *Board) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	c := b.conf
	set := c.settings()
	for k, v := range body {
		switch p, ok := set[k]; {
		case k == "HIRDMAN_AREAS":
			c.Areas = nil
			if err := json.Unmarshal([]byte(v), &c.Areas); err != nil {
				writeJSON(w, 400, map[string]string{"error": "HIRDMAN_AREAS: " + err.Error()})
				return
			}
		case !ok:
			writeJSON(w, 400, map[string]string{"error": "no setting " + k})
			return
		default:
			*p = strings.TrimSpace(v)
		}
	}
	if _, err := b.checkConfig(&c); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.headless() {
		writeJSON(w, 409, map[string]string{"error": "The agent is running a brief, a triage or a task; save once it ends"})
		return
	}
	if err := writeConfig(b.confPath, &c); err != nil {
		writeJSON(w, 500, map[string]string{"error": fmt.Sprintf("handleSaveSettings: %v", err)})
		return
	}
	writeJSON(w, 202, map[string]bool{"restarting": true})
	select {
	case b.restart <- struct{}{}:
	default:
	}
}

// writeConfig writes c to path, through a temporary file so a start never reads half of it.
func writeConfig(path string, c *Config) error {
	var data bytes.Buffer
	if err := toml.NewEncoder(&data).Encode(c); err != nil {
		return fmt.Errorf("writeConfig: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("writeConfig: %w", err)
	}
	if err := os.WriteFile(path+".tmp", data.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writeConfig: %w", err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return fmt.Errorf("writeConfig: %w", err)
	}
	return nil
}

// checkConfig refuses what main would stop on, or read as off without a word: a bad duration, key pattern, agent, MCP
// server or area; it returns the areas parsed.
func (b *Board) checkConfig(c *Config) ([]Area, error) {
	for k, v := range map[string]string{"briefEvery": c.BriefEvery, "jiraPoll": c.JiraPoll, "githubPoll": c.GitHubPoll} {
		if _, err := time.ParseDuration(v); v != "" && err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	if _, err := regexp.Compile(c.IssueKey); err != nil {
		return nil, fmt.Errorf("issueKey: %w", err)
	}
	if c.SessionIn != "" && c.SessionIn != "tab" && c.SessionIn != "window" {
		return nil, fmt.Errorf("sessionIn: %q, want tab or window", c.SessionIn)
	}
	for name, a := range c.Agents {
		if a.Kind != "claude-code" {
			return nil, fmt.Errorf("agents.%s: kind %q, want claude-code", name, a.Kind)
		}
	}
	unknown := func(name string) bool {
		_, ok := c.Agents[name]
		_, def := defaultAgents[name]
		return name != "" && !ok && !def
	}
	uses := map[string]Use{"use": c.Use}
	for i := range c.Areas {
		a := &c.Areas[i]
		uses["areas "+a.ID+": use"] = a.Use
		for i, r := range a.Runs {
			uses[fmt.Sprintf("areas %s: run %d", a.ID, i+1)] = Use{Headless: r.Agent}
		}
	}
	for where, u := range uses {
		for _, name := range []string{u.Session, u.Headless, u.Decide} {
			if unknown(name) {
				return nil, fmt.Errorf("%s: no agent %q in [agents]", where, name)
			}
		}
	}
	areas, err := b.parseAreas(c.Areas)
	if err != nil {
		return nil, err
	}
	return areas, checkMCP(c, areas)
}

// checkMCP refuses an [mcp] server that is not a command or a url, a name of none, and a feed the board can't read.
func checkMCP(c *Config, areas []Area) error {
	for name, s := range c.MCP {
		if (s.Command == "") == (s.URL == "") {
			return fmt.Errorf("mcp.%s: want a command or a url", name)
		}
	}
	unknown := func(names []string) string {
		i := slices.IndexFunc(names, func(n string) bool { _, ok := c.MCP[n]; return !ok })
		if i < 0 {
			return ""
		}
		return names[i]
	}
	for i := range areas {
		a := &areas[i]
		if n := unknown(a.MCP); n != "" {
			return fmt.Errorf("%s: mcp: no server %q in [mcp]", a.ID, n)
		}
		for j, r := range a.Runs {
			if n := unknown(r.MCP); n != "" {
				return fmt.Errorf("%s: run %d: mcp: no server %q in [mcp]", a.ID, j+1, n)
			}
		}
		for j, f := range a.Feeds {
			if _, err := regexp.Compile(f.Keys); err != nil {
				return fmt.Errorf("%s: feed %d: keys: %w", a.ID, j+1, err)
			}
			// shortcut: mcpCall speaks stdio only; give it the streamable HTTP transport when a feed needs a url server.
			if c.MCP[f.MCP].Command == "" || f.Tool == "" {
				return fmt.Errorf("%s: feed %d: want a tool of an [mcp] server with a command", a.ID, j+1)
			}
		}
	}
	return nil
}

// headless says whether the agent runs without a session: a brief, a triage or a task's run. The caller holds b.mu.
func (b *Board) headless() bool {
	if len(b.triage) > 0 {
		return true
	}
	for _, j := range b.st.Briefs {
		if j.Status == "running" {
			return true
		}
	}
	for _, t := range b.st.Tasks {
		if t.Job != nil && t.Job.Status == "running" && t.Job.Session == "" {
			return true
		}
	}
	return false
}

// seedLog creates the week files on their first start, from the history state.json already holds.
func (b *Board) seedLog() error {
	if _, err := os.Stat(b.logDir); !os.IsNotExist(err) {
		return nil
	}
	if err := os.MkdirAll(b.logDir, 0o700); err != nil {
		return fmt.Errorf("seedLog: %w", err)
	}
	var all []LogEntry
	for _, t := range b.st.Tasks {
		for _, e := range t.History {
			all = append(all, LogEntry{Entry: e, Task: t.ID, Title: t.Title, Area: t.Area})
		}
	}
	for _, e := range b.st.Log {
		all = append(all, LogEntry{Entry: e})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At.Before(all[j].At) })
	for i := range all {
		b.archive(&all[i])
	}
	return nil
}
