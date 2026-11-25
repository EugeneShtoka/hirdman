package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// mcpConn speaks newline-delimited JSON-RPC to a stdio MCP server, for one tool call.
type mcpConn struct {
	in   io.Writer
	sc   *bufio.Scanner
	tool string
}

func (c *mcpConn) send(msg map[string]any) error {
	msg["jsonrpc"] = "2.0"
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("%s: %w", c.tool, err)
	}
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("%s: send: %w", c.tool, err)
	}
	return nil
}

func (c *mcpConn) await(id int) (json.RawMessage, error) {
	for c.sc.Scan() {
		var r struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(c.sc.Bytes(), &r) != nil || r.ID == nil || *r.ID != id {
			continue
		}
		if r.Error != nil {
			return nil, fmt.Errorf("%s: %s", c.tool, r.Error.Message)
		}
		return r.Result, nil
	}
	if err := c.sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: server closed: %w", c.tool, err)
	}
	return nil, fmt.Errorf("%s: server closed", c.tool)
}

// call initializes the session, then calls the tool with args and returns its result.
func (c *mcpConn) call(args map[string]any) (json.RawMessage, error) {
	if err := c.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "hirdman", "version": "0"}}}); err != nil {
		return nil, err
	}
	if _, err := c.await(1); err != nil {
		return nil, err
	}
	if err := c.send(map[string]any{"method": "notifications/initialized"}); err != nil {
		return nil, err
	}
	if err := c.send(map[string]any{"id": 2, "method": "tools/call", "params": map[string]any{"name": c.tool, "arguments": args}}); err != nil {
		return nil, err
	}
	return c.await(2)
}

// mcpCall runs one tool call against a fresh stdio MCP server and returns its text.
func mcpCall(server MCPServer, tool string, args map[string]any) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, server.Command, server.Args...) //nolint:gosec // config.toml's own server
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	if err = cmd.Start(); err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	// Once the answer is in, how the server exits after stdin closes changes nothing.
	defer func() { _ = cmd.Wait() }()
	defer in.Close()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	raw, err := (&mcpConn{in: in, sc: sc, tool: tool}).call(args)
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err = json.Unmarshal(raw, &res); err != nil || len(res.Content) == 0 {
		return "", fmt.Errorf("%s: unexpected result %s", tool, raw)
	}
	if res.IsError {
		return "", fmt.Errorf("%s: %s", tool, res.Content[0].Text)
	}
	return res.Content[0].Text, nil
}

type feedMessage struct {
	EventID string `json:"event_id"`
	Sent    string `json:"sent"`
	Body    string `json:"body"`
}

// watchFeed polls one of an area's issue feeds and hands each new update to the agent, per issue.
func (b *Board) watchFeed(server MCPServer, area *Area, feed Feed, keyRe *regexp.Regexp, every time.Duration) {
	var lastOK time.Time
	failing := false
	for {
		if err := b.pollFeed(server, area, feed, keyRe); err != nil {
			log.Printf("issue feed %s: %v", area.ID, err)
			failing = true
		} else {
			if failing {
				b.kithBack(lastOK)
			}
			failing, lastOK = false, time.Now()
		}
		time.Sleep(every)
	}
}

// kithBack runs when kith reads work again (keyring unlocked, kithd back). An area's brief that started
// after the last good read may have run without chat, so it asks scheduleBrief to run it again.
func (b *Board) kithBack(lastOK time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for i := range b.areas {
		if id := b.areas[i].ID; b.lastBrief(id).After(lastOK) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return false
	}
	b.logf("kith", "kith reads work again; re-running the briefs that ran while it was down: %s", strings.Join(ids, ", "))
	b.saveOrLog()
	b.briefNow(ids...)
	return true
}

// briefNow asks scheduleBrief to brief these areas now. The caller holds b.mu.
func (b *Board) briefNow(ids ...string) {
	if b.rerun == nil {
		b.rerun = map[string]bool{}
	}
	for _, id := range ids {
		b.rerun[id] = true
	}
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Board) pollFeed(server MCPServer, area *Area, feed Feed, keyRe *regexp.Regexp) error {
	text, err := mcpCall(server, feed.Tool, feed.Args)
	if err != nil {
		return err
	}
	var read struct {
		Messages []feedMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(text), &read); err != nil {
		return fmt.Errorf("%s answer: %w", feed.Tool, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	seen, ok := b.st.JiraSeen[feed.id()]
	baseline := !ok
	ids := make([]string, 0, len(read.Messages))
	byKey := map[string][]string{}
	var keys []string
	for _, m := range read.Messages {
		ids = append(ids, m.EventID)
		if baseline || slices.Contains(seen, m.EventID) {
			continue
		}
		for _, key := range keyRe.FindAllString(m.Body, -1) {
			if _, ok := byKey[key]; !ok {
				keys = append(keys, key)
			}
			if !slices.Contains(byKey[key], m.Body) {
				byKey[key] = append(byKey[key], m.Body)
			}
		}
	}
	b.st.JiraSeen[feed.id()] = ids
	for _, key := range keys {
		update := strings.Join(byKey[key], "\n")
		var tracked bool
		for _, t := range b.st.Tasks {
			if !slices.Contains(t.Jira, key) {
				continue
			}
			tracked = true
			b.note(t, "jira", "update", update)
			if !b.busy(t) {
				go b.onUpdate(t.ID, update)
			}
		}
		if !tracked {
			go b.triageKey(area, key, update)
		}
	}
	return b.save()
}

// triageKey decides whether an untracked ticket belongs on the board (triage), one at a time: the model tier drops a key
// the feed text alone shows is not Eugene's, and the rest go to the headless agent, which looks it up in the area's Jira
// and adds it to the board when it concerns him.
func (b *Board) triageKey(area *Area, key, update string) {
	b.triage <- struct{}{}
	defer func() { <-b.triage }()
	d := b.decide("triage", &Task{Title: key, Area: area.ID, Jira: []string{key}}, update)
	if d.Answer != "agent" {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.logf("triage", "%s: %s", key, d)
		b.saveOrLog()
		return
	}
	prompt := fmt.Sprintf(`A Jira update mentions %[1]s, which has no task on Eugene's Hirdman board (%[2]s). Nobody is watching this run.

The update: %[3]s

Look %[1]s up in this folder's Jira. If it concerns Eugene (assigned to him, mentions or asks him, a ticket he reported,
commented on or is blocked by), add it to the board with area %[4]q, in the brief skill's format (step 5 of
.claude/skills/brief/SKILL.md in this folder):
  curl -s -X PUT %[2]s/api/tasks/<slug> -H 'Content-Type: application/json' -d '{...}'
with the whole story in summary, jira, jiraStatus, and messages when chat is involved. Otherwise change nothing.
Never send messages and never change Jira. Your final message is one line: what you decided and why.`, key, b.selfURL, update, area.ID)
	status, text := runAgent(b.agentFor("headless", area.ID, ""), area.Dir, prompt, b.mcpFlags(area.MCP)...)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logf("triage", "%s: %s; agent (%s): %s", key, d, status, text)
	b.saveOrLog()
}

// scheduleBrief briefs each area whose last brief is older than its briefEvery, or that briefNow named. An area's brief is
// the brief skill for work, the sweep (mail, calendar, personal chats) for personal, and the roadmap run for its roadmap
// files; the areas due together share one roadmap run.
func (b *Board) scheduleBrief() {
	for {
		b.mu.Lock()
		due := b.dueBriefs()
		if len(due) > 0 {
			if b.st.Briefs == nil {
				b.st.Briefs = map[string]*Job{}
			}
			for _, id := range due {
				b.st.Briefs[id] = &Job{Action: "brief", Status: "running", StartedAt: time.Now()}
			}
			b.saveOrLog()
		}
		b.mu.Unlock()

		if len(due) > 0 {
			b.runBriefs(due)
		}
		select {
		case <-b.wake:
		case <-time.After(10 * time.Minute):
		}
	}
}

// runBriefs runs the briefs of the due areas, one agent run after another: each area's prompt runs in its folder, then
// one roadmap run for all of them that have it. It ends each area's brief run with its status: failed if any of its agent
// runs failed.
func (b *Board) runBriefs(due []string) {
	status := map[string]string{}
	run := func(ag Agent, kind, dir, prompt string, flags []string, ids ...string) {
		s, text := runAgent(ag, dir, prompt, flags...)
		b.mu.Lock()
		defer b.mu.Unlock()
		b.logf(kind, "%s: %s", s, text)
		b.saveOrLog()
		for _, id := range ids {
			if status[id] != "failed" {
				status[id] = s
			}
		}
	}
	var roadmap []string
	for i := range b.areas {
		a := &b.areas[i]
		if !slices.Contains(due, a.ID) {
			continue
		}
		for _, r := range a.Runs {
			if r.Builtin == "roadmap" {
				roadmap = append(roadmap, a.ID)
			} else {
				mcp := r.MCP
				if mcp == nil {
					mcp = a.MCP
				}
				run(b.agentFor("headless", a.ID, r.Agent), cmp.Or(r.Name, "brief"), a.Dir, r.Prompt, b.mcpFlags(mcp), a.ID)
			}
		}
	}
	if prompt, ids := b.roadmapPrompt(roadmap); prompt != "" {
		run(b.agentFor("headless", "", ""), "roadmap", b.home, prompt, nil, ids...)
	}

	b.mu.Lock()
	now := time.Now()
	for _, id := range due {
		b.st.Briefs[id].Status, b.st.Briefs[id].EndedAt = cmp.Or(status[id], "ok"), &now
	}
	b.saveOrLog()
	b.mu.Unlock()
}

// dueBriefs is the areas to brief now: named by briefNow, or last briefed longer ago than their briefEvery; an area with
// no brief, or whose brief is running, is left out; a name for one that is running waits. The caller holds b.mu.
func (b *Board) dueBriefs() []string {
	var due []string
	for i := range b.areas {
		a := &b.areas[i]
		switch {
		case !b.briefs(a):
			delete(b.rerun, a.ID)
		case b.briefing(a.ID): // a name waits for the running brief to end
		case b.rerun[a.ID] || a.every > 0 && time.Since(b.lastBrief(a.ID)) >= a.every:
			delete(b.rerun, a.ID)
			due = append(due, a.ID)
		}
	}
	return due
}

// briefs says whether a has a brief: a prompt run, or the roadmap run and roadmap files for it.
func (b *Board) briefs(a *Area) bool {
	return slices.ContainsFunc(a.Runs, func(r Run) bool { return r.Builtin != "roadmap" }) ||
		slices.ContainsFunc(a.Runs, func(r Run) bool { return r.Builtin == "roadmap" }) && len(a.roadmapFiles()) > 0
}

// briefing says whether the area's brief is running. The caller holds b.mu.
func (b *Board) briefing(id string) bool {
	return b.st.Briefs[id] != nil && b.st.Briefs[id].Status == "running"
}

// roadmapPrompt asks the agent to keep one task per next item of the roadmap files of the areas ids names, and returns the
// areas that have files; "" when none has.
func (b *Board) roadmapPrompt(ids []string) (string, []string) {
	var files strings.Builder
	var with []string
	for i := range b.areas {
		if !slices.Contains(ids, b.areas[i].ID) {
			continue
		}
		for _, f := range b.areas[i].roadmapFiles() {
			fmt.Fprintf(&files, "- area %q: %s\n", b.areas[i].ID, f)
			if !slices.Contains(with, b.areas[i].ID) {
				with = append(with, b.areas[i].ID)
			}
		}
	}
	if files.Len() == 0 {
		return "", nil
	}
	return fmt.Sprintf(`Keep Eugene's Hirdman board (%s) in step with his roadmap files. This is a scheduled run and nobody is watching.

The files, by the board area their tasks belong to:
%s
1. GET /api/state. The tasks with source "roadmap" in these areas are yours; touch no other task.
2. Read each file and find its next to-do items: what it puts now or next (a Now/Next section, unchecked boxes, the
   top of an ordered list), not the whole backlog, at most five per file. Read only these files.
3. PUT /api/tasks/roadmap-<area>-<short slug of the item> (JSON, fields merge) for each one: source "roadmap", that area,
   a short title, group "roadmap", status "todo" and since today on create, and a summary that names the file's path
   and quotes the item. Send area only on create. An item the file no longer lists, or now marks done: its task goes to
   done, with a note (POST /api/tasks/<id>/notes {actor: "agent", kind: "note", text}) saying why.
4. A roadmap task Eugene marked done whose item the file still lists as open: mark it done in the file, the way that
   file marks done items (tick its box, move it to a done section, or remove it), change nothing else in the file,
   and note it on the task. A file git tracks (git ls-files --error-unmatch) gets that edit committed on its own:
   git commit -m "Roadmap: <item> done" -- <file>, never pushed. An untracked file is just edited.
   A dropped task: leave the file alone, and don't make the task again.

Your final message is one line: what changed on the board and in which files.`, b.selfURL, files.String()), with
}

// lastBrief is when the area's last brief started: a scheduled run, or for work also the brief the agent last pushed.
func (b *Board) lastBrief(id string) time.Time {
	var last time.Time
	if j := b.st.Briefs[id]; j != nil {
		last = j.StartedAt
	}
	if at, ok := b.st.Brief["at"].(string); ok && id == "work" {
		if t, err := time.Parse(time.RFC3339, at); err == nil && t.After(last) {
			last = t
		}
	}
	return last
}

// ghNotification is the part of a GitHub notification the board reads.
type ghNotification struct {
	Reason    string `json:"reason"`
	UpdatedAt string `json:"updated_at"`
	Subject   struct {
		Title            string `json:"title"`
		URL              string `json:"url"`
		LatestCommentURL string `json:"latest_comment_url"`
	} `json:"subject"`
}

// watchGitHub polls GitHub notifications every so often; never when every is 0.
func (b *Board) watchGitHub(keyRe *regexp.Regexp, every time.Duration) {
	if every <= 0 {
		return
	}
	log.Printf("reading GitHub notifications every %s", every)
	for ; ; time.Sleep(every) {
		if err := b.pollGitHub(keyRe); err != nil {
			log.Printf("github: %v", err)
		}
		if err := b.pollOwn(); err != nil {
			log.Printf("github: %v", err)
		}
		b.settleMerged(b.shippingChecks(), "")
		b.fireTriggers(true)
	}
}

// ghAPI is gh api's answer for args (the path last), as the gh user; what it printed comes back on an error too.
func ghAPI(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", append([]string{"api"}, args...)...).Output() //nolint:gosec // argv, no shell; GitHub's own urls
	if err != nil {
		return out, fmt.Errorf("gh api %s: %w", args[len(args)-1], err)
	}
	return out, nil
}

// pollGitHub notes each new GitHub notification on the tasks it is about (see ghTasks); no agent runs.
func (b *Board) pollGitHub(keyRe *regexp.Regexp) error {
	b.mu.Lock()
	since := b.st.GitHubSince
	if since == "" {
		defer b.mu.Unlock()
		b.st.GitHubSince = time.Now().UTC().Format(time.RFC3339)
		return b.save()
	}
	b.mu.Unlock()
	// all=true: a notification already read on github.com still reaches the board.
	query := "notifications?all=true&since=" + since
	// A 304 to If-None-Match is free of GitHub's rate limit (If-Modified-Since gets a 200 here), so polling often costs nothing.
	head, err := ghAPI("-i", "-H", "If-None-Match: "+b.ghETag, query)
	status, etag, perr := ghStatus(head)
	if perr == nil && status == http.StatusNotModified { // gh exits 1 on a 304
		return nil
	}
	if err = errors.Join(err, perr); err != nil {
		return fmt.Errorf("pollGitHub: %w", err)
	}
	all, err := ghNotifications(query)
	if err != nil {
		return fmt.Errorf("pollGitHub: %w", err)
	}
	slices.SortFunc(all, func(x, y ghNotification) int { return strings.Compare(x.UpdatedAt, y.UpdatedAt) })
	newest := since
	type update struct {
		n    *ghNotification
		text string
	}
	var updates []update
	for i := range all {
		n := &all[i]
		// shortcut: a notification updated in the same second as the last poll's newest is skipped; dedupe by id if that ever bites.
		if n.UpdatedAt <= since || n.Subject.URL == "" {
			continue
		}
		newest = max(newest, n.UpdatedAt)
		updates = append(updates, update{n, ghUpdate(n)})
	}
	b.mu.Lock()
	var settle []settleCheck
	for _, u := range updates {
		for _, t := range b.ghTasks(u.n, keyRe) {
			b.note(t, "github", "update", u.text)
			if page := ghPageURL(u.n.Subject.URL); slices.Contains(taskPulls(t), page) {
				settle = append(settle, settleCheck{t.ID, page})
			}
		}
	}
	b.st.GitHubSince = newest
	b.ghETag = etag
	err = b.save()
	b.mu.Unlock()
	b.settleMerged(settle, since)
	return err
}

// ghEvent is the part of a GitHub event pollOwn reads.
type ghEvent struct {
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Payload   struct {
		Action      string `json:"action"`
		PullRequest struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	} `json:"payload"`
}

// pollOwn settles the tasks linking a pull request the gh user merged or closed: GitHub notifies nobody of
// their own actions, so pollGitHub never hears of those. No agent runs.
// shortcut: one page of events (100), and one that surfaces after a newer one was seen is skipped; dedupe by event id if that bites.
func (b *Board) pollOwn() error {
	b.mu.Lock()
	since := b.st.GitHubOwnSince
	if since == "" {
		defer b.mu.Unlock()
		b.st.GitHubOwnSince = time.Now().UTC().Format(time.RFC3339)
		return b.save()
	}
	b.mu.Unlock()
	if b.ghLogin == "" {
		out, err := ghAPI("--jq", ".login", "user")
		if err != nil {
			return fmt.Errorf("pollOwn: %w", err)
		}
		b.ghLogin = strings.TrimSpace(string(out))
	}
	events, etag, err := ghEvents(b.ghLogin, b.ghOwn)
	if err != nil || events == nil {
		return err
	}
	newest := since
	var settle []settleCheck
	b.mu.Lock()
	for _, e := range events {
		if e.CreatedAt <= since {
			continue
		}
		newest = max(newest, e.CreatedAt)
		if e.Type != "PullRequestEvent" || e.Payload.Action != "merged" && e.Payload.Action != "closed" {
			continue
		}
		page := ghPageURL(e.Payload.PullRequest.URL)
		for _, t := range b.st.Tasks {
			if slices.Contains(taskPulls(t), page) {
				settle = append(settle, settleCheck{t.ID, page})
			}
		}
	}
	b.st.GitHubOwnSince = newest
	b.ghOwn = etag
	err = b.save()
	b.mu.Unlock()
	b.settleMerged(settle, since)
	return err
}

// ghEvents is user's newest events and their ETag; nil while they match etag (a 304, free of the rate limit).
func ghEvents(user, etag string) ([]ghEvent, string, error) {
	query := "users/" + user + "/events?per_page=100"
	head, err := ghAPI("-i", "-H", "If-None-Match: "+etag, query)
	status, etag, perr := ghStatus(head)
	if perr == nil && status == http.StatusNotModified { // gh exits 1 on a 304
		return nil, etag, nil
	}
	if err = errors.Join(err, perr); err != nil {
		return nil, "", fmt.Errorf("ghEvents: %w", err)
	}
	out, err := ghAPI(query)
	events := []ghEvent{}
	if err == nil {
		err = json.Unmarshal(out, &events)
	}
	if err != nil {
		return nil, "", fmt.Errorf("ghEvents: %w", err)
	}
	return events, etag, nil
}

var pullRe = regexp.MustCompile(`https://github\.com/[^/\s]+/[^/\s]+/pull/\d+`)

// linkPulls adds to t's links each pull request page text names that t doesn't link yet, so the
// GitHub notifications about it reach t (see ghTasks).
func linkPulls(t *Task, text string) {
	have := taskPulls(t)
	for _, p := range pullRe.FindAllString(text, -1) {
		if !slices.Contains(have, p) {
			have = append(have, p)
			t.Links = append(t.Links, map[string]string{"label": "PR #" + p[strings.LastIndex(p, "/")+1:], "url": p})
		}
	}
}

// taskPulls are the github.com pages of the pull requests t links, once each.
func taskPulls(t *Task) []string {
	var pulls []string
	for _, l := range t.Links {
		if p := pullRe.FindString(l["url"]); p != "" && !slices.Contains(pulls, p) {
			pulls = append(pulls, p)
		}
	}
	return pulls
}

// ghPull is the part of a pull request settleMerged and cleanMerged read.
type ghPull struct {
	State    string `json:"state"` // open | closed
	Merged   bool   `json:"merged"`
	ClosedAt string `json:"closed_at"`
	MergedAt string `json:"merged_at"`
	Base     struct {
		Ref  string `json:"ref"`
		Repo struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repo"`
	} `json:"base"`
	// Shipped is whether a merged pull request is on its repository's default branch (see ghShipped).
	Shipped bool `json:"-"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

// ghMerge is the part of a pull request onBranch reads.
type ghMerge struct {
	Number         int    `json:"number"`
	Merged         bool   `json:"merged"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// ghGet reads gh api's JSON answer for path into v.
func ghGet(path string, v any) error {
	out, err := ghAPI(path)
	if err == nil {
		err = json.Unmarshal(out, v)
	}
	if err != nil {
		return fmt.Errorf("ghGet: %w", err)
	}
	return nil
}

// onBranch is whether the pull request at page has reached branch: merged into it, or its merge commit
// carried there by a pull request merged into branch after it. That one may be squash-merged (the be repo's
// development -> main releases are), so it is its commit list that is read, not branch's history.
func onBranch(page, branch string) (bool, error) {
	var p ghMerge
	if err := ghGet(ghAPIURL(page), &p); err != nil {
		return false, err
	}
	if !p.Merged || p.Base.Ref == branch {
		return p.Merged, nil
	}
	api := ghAPIURL(page)
	repo := api[:strings.LastIndex(api, "/pulls/")]
	// shortcut: the 30 pull requests into branch updated last, and each one's first 100 commits; page further if releases outgrow that.
	var into []ghMerge
	if err := ghGet(repo+"/pulls?state=closed&sort=updated&direction=desc&base="+url.QueryEscape(branch), &into); err != nil {
		return false, err
	}
	for _, r := range into {
		if r.MergedAt == "" || r.MergedAt < p.MergedAt {
			continue
		}
		var commits []struct {
			SHA string `json:"sha"`
		}
		if err := ghGet(fmt.Sprintf("%s/pulls/%d/commits?per_page=100", repo, r.Number), &commits); err != nil {
			return false, err
		}
		for _, c := range commits {
			if c.SHA == p.MergeCommitSHA {
				return true, nil
			}
		}
	}
	return false, nil
}

// due says who made a trigger that needs no GitHub call due, and how: its time came, or one of the task's Jira keys
// is in its status (as the brief and update sessions last wrote it); "" when it isn't due.
func (tr Trigger) due(t *Task, now time.Time) (actor, why string) {
	if tr.At != nil && !now.Before(*tr.At) {
		return "board", "its time came, " + tr.At.Local().Format("2006-01-02 15:04")
	}
	for _, k := range t.Jira {
		if tr.JiraStatus != "" && strings.EqualFold(t.JiraStatus[k], tr.JiraStatus) {
			return "jira", k + " reached " + t.JiraStatus[k]
		}
	}
	return "", ""
}

// fireTriggers starts a session for each trigger that is due (see due) and, when gh, each trigger whose branch a
// pull request its task links has reached (see onBranch). A task busy with another session keeps its trigger for a
// later check; a dropped task's never fire.
// shortcut: every pending branch trigger costs a few GitHub calls each poll, fine for a handful; check only on new notifications if they pile up.
func (b *Board) fireTriggers(gh bool) {
	type check struct {
		id    string
		tr    Trigger
		pages []string
	}
	var checks []check
	b.mu.Lock()
	for _, t := range b.st.Tasks {
		for _, tr := range t.Triggers {
			if tr.Fired != nil || t.Status == "dropped" {
				continue
			}
			if actor, why := tr.due(t, time.Now()); why != "" {
				b.fire(t, tr, actor, why)
			} else if pages := taskPulls(t); gh && tr.On != "" && len(pages) > 0 {
				checks = append(checks, check{t.ID, tr, pages})
			}
		}
	}
	b.mu.Unlock()
	for _, c := range checks {
		reached := false
		for _, page := range c.pages {
			ok, err := onBranch(page, c.tr.On)
			if err != nil {
				log.Printf("github: trigger %s: %v", c.id, err)
			}
			if reached = ok; ok {
				break
			}
		}
		if !reached {
			continue
		}
		b.mu.Lock()
		// Re-checked: Eugene may have changed the triggers, or a session started, while GitHub answered.
		if t := b.st.Tasks[c.id]; t != nil {
			b.fire(t, c.tr, "github", "a pull request it links reached "+c.tr.On)
		}
		b.mu.Unlock()
	}
}

// fire starts tr's session on t, unless t no longer has tr or is busy with another session. The caller holds b.mu.
func (b *Board) fire(t *Task, tr Trigger, actor, why string) {
	i := slices.Index(t.Triggers, tr)
	if i < 0 || b.busy(t) {
		return
	}
	now := time.Now()
	t.Triggers[i].Fired = &now
	text := why + "; do: " + tr.Do
	b.note(t, actor, "trigger", text)
	b.start(t, actRequest{Action: "trigger", Comment: text})
	b.saveOrLog()
}

// settleCheck is a task a notification reached through a pull request it links: that pull request's page,
// or "" for a task re-checked because it is shipping.
type settleCheck struct{ id, page string }

// shippingChecks re-check each open task whose merged pull requests had not reached the default branch yet:
// the release that carries them there notifies nobody.
// shortcut: every poll re-reads each shipping task's pull requests and releases (a few gh calls per task); key off the default branch's ETag if that gets heavy.
func (b *Board) shippingChecks() []settleCheck {
	b.mu.Lock()
	defer b.mu.Unlock()
	var checks []settleCheck
	for _, t := range b.st.Tasks {
		if t.Shipping && t.Status != "done" && t.Status != "dropped" {
			checks = append(checks, settleCheck{t.ID, ""})
		}
	}
	return checks
}

// pullsShipped is whether each merged pull request is on its default branch.
func pullsShipped(pulls []ghPull) bool {
	return !slices.ContainsFunc(pulls, func(p ghPull) bool { return p.Merged && !p.Shipped })
}

// ghShipped is whether merged p (at page) is on its default branch: merged into it, or carried there by a
// release, a later merged pull request from p's base into the default branch. Releases are often
// squash-merged, so p's merge commit is not an ancestor of the default branch to check.
func ghShipped(page string, p *ghPull) (bool, error) {
	def := p.Base.Repo.DefaultBranch
	if p.Base.Ref == def {
		return true, nil
	}
	api := ghAPIURL(page)
	owner := strings.Split(page, "/")[3]
	out, err := ghAPI(api[:strings.LastIndex(api, "/pulls/")] + "/pulls?state=closed&sort=updated&direction=desc&per_page=20" +
		"&base=" + url.QueryEscape(def) + "&head=" + url.QueryEscape(owner+":"+p.Base.Ref))
	var releases []ghPull
	if err == nil {
		err = json.Unmarshal(out, &releases)
	}
	if err != nil {
		return false, fmt.Errorf("ghShipped: %w", err)
	}
	return released(p, releases), nil
}

// released is whether one of releases merged at or after p did.
func released(p *ghPull, releases []ghPull) bool {
	return slices.ContainsFunc(releases, func(r ghPull) bool { return r.MergedAt != "" && r.MergedAt >= p.MergedAt })
}

// pullsDone is whether every pull request is closed and one of them merged; all closed
// unmerged (abandoned, or replaced by one the task doesn't link yet) leaves the task alone.
func pullsDone(pulls []ghPull) bool {
	merged := false
	for i := range pulls {
		if pulls[i].State != "closed" {
			return false
		}
		merged = merged || pulls[i].Merged
	}
	return merged
}

// settleMerged moves each open task to done once pullsDone holds for the pull requests it links, when the
// notification's own pull request closed after since: a reopened task, or one linking long-merged pull
// requests as context, stays open through later comments on them. Merged ones count once on their default
// branch: until then the task is shipping, re-checked each poll. A task waiting for more than their
// review (see endsWithPulls) only gets a note. No agent runs.
func (b *Board) settleMerged(checks []settleCheck, since string) {
	for _, c := range checks {
		b.mu.Lock()
		var pages []string
		shipping := false
		if t := b.st.Tasks[c.id]; t != nil && t.Status != "done" && t.Status != "dropped" {
			pages, shipping = taskPulls(t), t.Shipping
		}
		b.mu.Unlock()
		if len(pages) == 0 {
			continue
		}
		pulls := settlePulls(c.id, pages)
		i := slices.Index(pages, c.page)
		fresh := shipping || i >= 0 && pulls != nil && pulls[i].ClosedAt > since
		if fresh && pullsDone(pulls) {
			b.settle(c.id, pulls)
		}
	}
}

// settlePulls are the pull requests at pages, with Shipped set; nil unless every one was read.
func settlePulls(id string, pages []string) []ghPull {
	pulls := make([]ghPull, len(pages))
	for i, page := range pages {
		p := &pulls[i]
		out, err := ghAPI(ghAPIURL(page))
		if err == nil {
			err = json.Unmarshal(out, p)
		}
		if err == nil && p.Merged {
			p.Shipped, err = ghShipped(page, p)
		}
		if err != nil {
			log.Printf("github: settle %s: %v", id, err)
			return nil
		}
	}
	return pulls
}

// settle is what done pulls make of task id, if still open: shipping while a merged one is off its default
// branch, else a note when it waits for more than them (close-on-merge), else done.
func (b *Board) settle(id string, pulls []ghPull) {
	var d *Decision
	if pullsShipped(pulls) {
		if view, ok := b.snapshot(id); ok && view.Status == "waiting" {
			d = new(b.decide("close-on-merge", &view, ""))
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// Re-checked: Eugene may have moved it while GitHub answered.
	t := b.st.Tasks[id]
	if t != nil && d != nil {
		b.note(t, "board", "decision", d.String())
	}
	switch {
	case t == nil || t.Status == "done" || t.Status == "dropped":
		return
	case !pullsShipped(pulls):
		if t.Shipping {
			return
		}
		t.Shipping = true
		p := &pulls[slices.IndexFunc(pulls, func(p ghPull) bool { return p.Merged && !p.Shipped })]
		b.note(t, "github", "update", "Its pull requests are merged into "+p.Base.Ref+", not "+p.Base.Repo.DefaultBranch+" yet; checked again each poll")
	case t.Status == "waiting" && (d == nil || d.Answer != "close"):
		t.Shipping = false
		b.note(t, "github", "update", "Its pull requests are merged or closed; still waiting for "+t.WaitingFor)
	default:
		b.note(t, "github", "status", t.Status+" → done: its pull requests are merged or closed")
		setStatus(t, "done")
		b.doneNow(t)
	}
	b.saveOrLog()
}

var reviewRe = regexp.MustCompile(`(?i)^(PR|pull request|review|merge)\b`)

// endsWithPulls is whether what a waiting task waits for ends with its pull requests: nothing, or their
// review ("PR review", "PR #27 review", "review of #618 and #620"). Anything else (a deploy, an answer, a check on prod) keeps the
// task open once they merge.
func endsWithPulls(waitingFor string) bool {
	return waitingFor == "" || reviewRe.MatchString(waitingFor)
}

// question is one decision the board asks of decide: Ask, the model's question about the task, answered with one of Opts;
// Parse, the Go code that answers it ("" is unsure), nil for none; Agent, the option that hands it to a full agent run,
// "" for none; Fallback, today's answer, when no tier is sure.
type question struct {
	ask      string
	opts     []string
	parse    func(t *Task, context string) (answer, why string)
	agent    string
	fallback string
}

var questions = map[string]question{
	"update-needs-agent": {
		ask: `A line from the Jira feed arrived about one of the task's tickets (the context). Does it need the update agent, a
full run that reads the ticket in Jira and rewrites the task's story, or is the line in the task's history enough?
"note" when it changes nothing the story or Eugene's next step depends on: a bot or automation moving the ticket the way
the story expects, a field edit, someone else's routine comment or subtask. "agent" when it asks something of Eugene (a
mention, a question, an assignment), or changes the ticket's status, scope or owner in a way the story does not say.`,
		opts: []string{"note", "agent"}, agent: "agent", fallback: "agent",
	},
	"triage": {
		ask: `A line from the Jira feed mentions a ticket (the task's jira) that has no task on the board yet; the task is only
the candidate. Does the ticket belong on Eugene's board? "drop" when the line alone shows it is not his: someone else's
ticket moved, assigned or commented on, with no mention of Eugene, no question or assignment to him. "agent" when it
might be his (it names or asks him, assigns or reports to him, or the line does not say whose it is): the triage agent
then looks the ticket up in Jira and adds it when it concerns him.`,
		opts: []string{"drop", "agent"}, agent: "agent", fallback: "agent",
	},
	"close-on-merge": {
		ask: `All the task's pull requests are merged or closed, and the task is waiting (waitingFor). Did that wait end with
them, so the task is done ("close"), or does it wait for more: a deploy, someone's answer, a check on prod ("wait")?`,
		opts: []string{"close", "wait"}, fallback: "wait",
		parse: func(t *Task, _ string) (string, string) {
			if endsWithPulls(t.WaitingFor) {
				return "close", fmt.Sprintf("waiting for %q ends with its pull requests", t.WaitingFor)
			}
			return "", fmt.Sprintf("%q is not a review", t.WaitingFor)
		},
	},
}

// Decision is decide's answer, the tier that gave it ("default" when none was sure) and why, with the tiers before it.
type Decision struct {
	Question, Answer, Tier, Why string
}

func (d Decision) String() string {
	return fmt.Sprintf("%s: %s (%s) %s", d.Question, d.Answer, d.Tier, d.Why)
}

// sure is the confidence the model tier must give to be taken.
const sure = 0.7

// decide answers question q about task t (a snapshot: decide runs without mu) and context by its chain of tiers, each
// until one is sure: "parse", the question's Go code; "model", the area's decide agent asked for JSON, without tools;
// "agent", the full agent run the question can hand it to. A tier that fails or is unsure passes it on; past the last,
// the answer is the question's fallback, today's behavior.
func (b *Board) decide(q string, t *Task, context string) Decision {
	qu := questions[q]
	var notes []string
	for _, tier := range b.chain(q) {
		var answer, why string
		switch tier {
		case "parse":
			if qu.parse == nil {
				continue
			}
			answer, why = qu.parse(t, context)
		case "model":
			var err error
			if answer, why, err = b.askModel(&qu, t, context); err != nil {
				answer, why = "", "failed: "+err.Error()
			}
		case "agent":
			answer, why = qu.agent, "handed to the agent"
		}
		if slices.Contains(qu.opts, answer) {
			return Decision{q, answer, tier, strings.Join(append(notes, why), "; ")}
		}
		notes = append(notes, tier+" unsure: "+why)
	}
	return Decision{q, qu.fallback, "default", strings.Join(append(notes, "today's behavior"), "; ")}
}

// askModel puts qu to the decide agent of t's area: one answer of qu.opts, or "" when it is unsure.
func (b *Board) askModel(qu *question, t *Task, context string) (answer, why string, err error) {
	view := *t
	view.Job, view.History = nil, nil
	task, err := json.Marshal(view)
	if err != nil {
		return "", "", fmt.Errorf("askModel: %w", err)
	}
	opts, _ := json.Marshal(qu.opts) //nolint:errchkjson // strings only
	prompt := fmt.Sprintf(`You answer one question about a task on Eugene's Hirdman board, a task board.

%s

Task:
%s

Context: %s

Reply with only a JSON object: {"answer": one of %s, or "unsure", "why": "<one short line>", "confidence": <0 to 1>}`,
		qu.ask, task, cmp.Or(context, "none"), opts)
	status, text := runAgent(b.agentFor("decide", t.Area, ""), b.home, prompt, "--tools", "", "--strict-mcp-config")
	if status != "ok" {
		return "", "", fmt.Errorf("askModel: %s", text)
	}
	var a struct {
		Answer     string  `json:"answer"`
		Why        string  `json:"why"`
		Confidence float64 `json:"confidence"`
	}
	// The model may wrap it in a code fence.
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i < 0 || j < i || json.Unmarshal([]byte(text[i:j+1]), &a) != nil {
		return "", "", fmt.Errorf("askModel: not a JSON answer: %q", text)
	}
	if a.Confidence < sure {
		return "", fmt.Sprintf("%s (confidence %.2f)", a.Why, a.Confidence), nil
	}
	return a.Answer, a.Why, nil
}

// snapshot is a copy of task id that decide can read without mu.
func (b *Board) snapshot(id string) (Task, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var view Task
	t := b.st.Tasks[id]
	if t == nil {
		return view, false
	}
	data, err := json.Marshal(t)
	return view, err == nil && json.Unmarshal(data, &view) == nil
}

// onUpdate is a Jira feed line on task id, already in its history: the update agent runs when update-needs-agent says so.
func (b *Board) onUpdate(id, update string) {
	view, ok := b.snapshot(id)
	if !ok {
		return
	}
	d := b.decide("update-needs-agent", &view, update)
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.st.Tasks[id]
	if t == nil {
		return
	}
	b.note(t, "board", "decision", d.String())
	if d.Answer == "agent" && !b.busy(t) {
		b.start(t, actRequest{Action: "update", Comment: update})
	}
	b.saveOrLog()
}

// doneNow is t moving to done: its Jira tickets are checked to have moved on too (see checkJira), and with
// HIRDMAN_CLEAN_MERGED, what its merged pull requests leave behind goes (see cleanMerged); callers hold mu.
func (b *Board) doneNow(t *Task) {
	if len(t.Jira) > 0 {
		if dir, err := b.dirFor(t); err != nil {
			b.note(t, "board", "qa", "No Jira check: "+err.Error())
		} else {
			go b.checkJira(b.agentFor("decide", t.Area, ""), t.ID, t.Title, dir, t.Jira, b.mcpFor(t.Area))
		}
	}
	pages := taskPulls(t)
	if !b.clean || len(pages) == 0 {
		return
	}
	pane := ""
	// A session still running has not reported yet: it may be mid-work, so it stays.
	if t.Job != nil && t.Job.Status != "running" {
		pane = t.Job.Pane
	}
	dir, err := b.dirFor(t)
	if err != nil {
		b.note(t, "board", "cleanup", "No cleanup: "+err.Error())
		return
	}
	go b.cleanMerged(t.ID, dir, pane, pages)
}

// checkJira runs once a task is done: a run of agent ag in the task's folder (dir) reads each of its Jira keys live and
// moves one still before QA on, the way the qa action does, noting on task id what it found. It leaves the task's
// session alone, so a session that closed its own task keeps working. flags are the agent's, its MCP servers.
func (b *Board) checkJira(ag Agent, id, title, dir string, keys []string, flags []string) {
	prompt := fmt.Sprintf(`The task %[1]q on Eugene's Hirdman board (%[2]s) was just closed as done. Nobody is watching this run.

Its Jira tickets should have moved on with it. Read the status of each in Jira now: %[3]s. One still before QA
(backlog, to do, in progress, dev done) moves to "QA - Staging" (read its transitions first); a sub-task that has no
QA state goes straight to Done. Leave a ticket in QA or past it as it is, and change nothing else in Jira or on the board.
Then record each key's status now:
  curl -s -X PUT %[2]s/api/tasks/%[4]s -H 'Content-Type: application/json' -d '{"jiraStatus": {"<KEY>": "<status now>"}}'
Your final message goes into the task's history as is: one line per key, its status and whether you moved it.`,
		title, b.selfURL, strings.Join(keys, ", "), id)
	status, text := runAgent(ag, dir, prompt, flags...)
	if status != "ok" {
		text = "Jira check failed: " + text
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.st.Tasks[id]; t != nil {
		b.note(t, "agent", "qa", text)
		b.saveOrLog()
	}
}

// prep runs once a task is added, whoever adds it: a look-only agent run in its folder checks the story's facts and
// gathers what deciding on it needs, and its findings go into the task's history, where the task's sessions read them.
// One runs at a time, as a brief can add many tasks at once.
func (b *Board) prep(id string) {
	b.prepping.Lock()
	defer b.prepping.Unlock()
	area, dir, prompt := b.prepPrompt(id)
	if prompt == "" {
		return
	}
	status, text := runAgent(b.agentFor("headless", area, ""), dir, prompt)
	if status != "ok" {
		text = "Preprocessing failed: " + text
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.st.Tasks[id]; t != nil {
		b.note(t, "agent", "prep", text)
		b.saveOrLog()
	}
}

// prepPrompt is prep's area, folder and prompt for task id; no prompt for a task closed or gone before its turn.
func (b *Board) prepPrompt(id string) (area, dir, prompt string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.st.Tasks[id]
	if t == nil || t.Status == "done" || t.Status == "dropped" {
		return "", "", ""
	}
	dir, err := b.dirFor(t)
	var task []byte
	if err == nil {
		task, err = json.Marshal(t)
	}
	if err != nil {
		b.note(t, "board", "prep", "No preprocessing: "+err.Error())
		b.saveOrLog()
		return "", "", ""
	}
	return t.Area, dir, fmt.Sprintf(`A task was just added to Eugene's Hirdman board (%s); its source says by whom ("" is the brief). Nobody is
watching this run.

Task:
%s

Before Eugene gets to it, do the first investigation, so deciding how to proceed is quick. Check the facts its story
states against their sources, and find what it touches: code, config, docs, git history, open pull requests; Jira
tickets and chat (kith) when it names or implies them; logs and databases this folder's tools and CLAUDE.md reach, with
read-only queries; public docs when it is about an outside API or tool. Then what is already there or already tried,
the likely approaches and their size, and what is unclear and who could answer it. Look only: change no file, write to
no database, send no message, change nothing in Jira or on the board.
Your final message goes into the task's history as is: the facts, with file:line, keys and links, then the open
questions, in at most 15 short lines.`, b.selfURL, task)
}

// cleanMerged runs once a task is done: if one of its pull requests (pages) is merged, it closes the
// task's session (its wezterm pane, "" for none), then runs pruneWorktree in dir for each merged one,
// noting on task id what it did.
func (b *Board) cleanMerged(id, dir, pane string, pages []string) {
	say := func(text string) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if t := b.st.Tasks[id]; t != nil {
			b.note(t, "board", "cleanup", text)
			b.saveOrLog()
		}
	}
	var merged []ghPull
	for _, page := range pages {
		out, err := ghAPI(ghAPIURL(page))
		var p ghPull
		if err == nil {
			err = json.Unmarshal(out, &p)
		}
		if err != nil {
			log.Printf("github: clean %s: %v", id, err)
			continue
		}
		if p.Merged {
			merged = append(merged, p)
		}
	}
	if len(merged) == 0 {
		return
	}
	if pane != "" && paneAlive(pane) {
		if _, err := wezterm([]string{"cli", "kill-pane", "--pane-id", pane}); err != nil {
			log.Printf("github: clean %s: %v", id, err)
		} else {
			say("closed its session (wezterm pane " + pane + "): its pull request is merged")
			// shortcut: a session slower than this to exit keeps its worktree (noted as in use); poll inUse if that shows up.
			time.Sleep(time.Second)
		}
	}
	for i := range merged {
		text, err := pruneWorktree(dir, merged[i].Head.Ref, merged[i].Head.SHA)
		if err != nil {
			log.Printf("github: clean %s: %v", id, err)
		}
		if text != "" {
			say(text)
		}
	}
}

// pruneWorktree removes branch's worktree from dir/.worktrees/, and then branch, only when nothing in it
// can be lost or is in use: it is still at sha (the merged head), has nothing uncommitted, isn't locked and
// no process runs in it. It returns what it did for the task's history, "" when no worktree there has branch.
func pruneWorktree(dir, branch, sha string) (string, error) {
	dir, err := filepath.EvalSymlinks(dir) // git lists real paths
	if err != nil {
		return "", fmt.Errorf("pruneWorktree: %w", err)
	}
	path, head, locked, err := worktreeOf(dir, branch)
	if path == "" || err != nil {
		return "", err
	}
	name := ".worktrees/" + filepath.Base(path)
	status, err := git(path, "status", "--porcelain")
	switch {
	case err != nil:
		return "", fmt.Errorf("pruneWorktree: %w", err)
	case locked:
		return "kept " + name + ": it is locked", nil
	case head != sha:
		return "kept " + name + ": " + branch + " has moved on from the merged head", nil
	case status != "":
		return "kept " + name + ": it has uncommitted changes", nil
	case inUse(path):
		return "kept " + name + ": a process (a session?) is running in it", nil
	}
	// No --force: git refuses anything it would lose, should that change since the checks.
	if _, err := git(dir, "worktree", "remove", path); err != nil {
		return "", fmt.Errorf("pruneWorktree: %w", err)
	}
	if _, err := git(dir, "branch", "-D", branch); err != nil {
		return "removed " + name + ", merged; kept branch " + branch, fmt.Errorf("pruneWorktree: %w", err)
	}
	return "removed " + name + " and branch " + branch + ", merged", nil
}

// worktreeOf is the worktree directly in dir/.worktrees/ that has branch checked out: its path ("" when
// there is none), its HEAD and whether it is locked.
func worktreeOf(dir, branch string) (path, head string, locked bool, err error) {
	list, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", "", false, fmt.Errorf("worktreeOf: %w", err)
	}
	for block := range strings.SplitSeq(list, "\n\n") {
		path, head, locked = "", "", false
		on := false
		for line := range strings.SplitSeq(block, "\n") {
			k, v, _ := strings.Cut(line, " ")
			switch k {
			case "worktree":
				path = v
			case "HEAD":
				head = v
			case "branch":
				on = v == "refs/heads/"+branch
			case "locked":
				locked = true
			}
		}
		if on && filepath.Dir(path) == filepath.Join(dir, ".worktrees") {
			return path, head, locked, nil
		}
	}
	return "", "", false, nil
}

// inUse is whether some process's working folder is path or inside it; true when /proc can't tell.
func inUse(path string) bool {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, p := range procs {
		cwd, err := os.Readlink("/proc/" + p.Name() + "/cwd")
		if err == nil && (cwd == path || strings.HasPrefix(cwd, path+"/")) {
			return true
		}
	}
	return false
}

// git is git's output for args, run in dir; what it printed comes back on an error.
func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput() //nolint:gosec // argv, no shell; the board's own folders and GitHub's branch names
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// ghAPIURL is the API url of a github.com page: github.com/o/r/pull/7 -> .../repos/o/r/pulls/7.
func ghAPIURL(page string) string {
	return strings.Replace(strings.Replace(page, "://github.com/", "://api.github.com/repos/", 1), "/pull/", "/pulls/", 1)
}

// ghNotifications reads every page of a notifications query.
func ghNotifications(query string) ([]ghNotification, error) {
	out, err := ghAPI("--paginate", query)
	if err != nil {
		return nil, err
	}
	var all []ghNotification
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); { // --paginate prints one array per page
		var page []ghNotification
		if err := dec.Decode(&page); err != nil {
			return nil, fmt.Errorf("ghNotifications: %w", err)
		}
		all = append(all, page...)
	}
	return all, nil
}

// ghStatus is the status and ETag of a gh api -i answer.
func ghStatus(out []byte) (int, string, error) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(out)), nil)
	if err != nil {
		return 0, "", fmt.Errorf("ghStatus: %w", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Etag"), nil
}

// ghPageURL is the github.com page of an API url: .../repos/o/r/pulls/7 -> github.com/o/r/pull/7.
func ghPageURL(api string) string {
	return strings.Replace(strings.Replace(api, "://api.github.com/repos/", "://github.com/", 1), "/pulls/", "/pull/", 1)
}

// ghTasks are the tasks a notification is about: those linking its pull request or issue, or tracking
// a Jira key its title names.
func (b *Board) ghTasks(n *ghNotification, keyRe *regexp.Regexp) []*Task {
	page := ghPageURL(n.Subject.URL)
	keys := keyRe.FindAllString(n.Subject.Title, -1)
	var tasks []*Task
	for _, t := range b.st.Tasks {
		linked := slices.ContainsFunc(t.Links, func(l map[string]string) bool {
			return l["url"] == page || strings.HasPrefix(l["url"], page+"/") || strings.HasPrefix(l["url"], page+"#")
		})
		if linked || slices.ContainsFunc(keys, func(k string) bool { return slices.Contains(t.Jira, k) }) {
			// A pull request that reaches t by a Jira key in its title becomes t's, so its merge settles t and fires t's triggers.
			linkPulls(t, page)
			tasks = append(tasks, t)
		}
	}
	return tasks
}

// ghUpdate is the history line for a notification: why it came, the page, and what happened last,
// the newest comment or the pull request's state.
func ghUpdate(n *ghNotification) string {
	text := fmt.Sprintf("%s: %s %s", n.Reason, n.Subject.Title, ghPageURL(n.Subject.URL))
	if n.Subject.LatestCommentURL == "" {
		return text
	}
	out, err := ghAPI(n.Subject.LatestCommentURL)
	if err != nil {
		log.Printf("github: %v", err)
		return text
	}
	var last struct {
		User   struct{ Login string } `json:"user"`
		Body   string                 `json:"body"`
		State  string                 `json:"state"`
		Merged bool                   `json:"merged"`
	}
	if err := json.Unmarshal(out, &last); err != nil {
		log.Printf("github: %s: %v", n.Subject.LatestCommentURL, err)
		return text
	}
	if n.Subject.LatestCommentURL == n.Subject.URL { // no comment: the subject itself changed
		if last.Merged {
			last.State = "merged"
		}
		return text + "\nnow " + last.State
	}
	body := []rune(last.Body)
	if len(body) > 500 {
		body = append(body[:500], '…')
	}
	return text + "\n" + last.User.Login + ": " + string(body)
}

func envDuration(key string) time.Duration {
	d, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return 0
	}
	return d
}
