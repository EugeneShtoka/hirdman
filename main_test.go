package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWeekOf(t *testing.T) {
	for day, want := range map[string]string{
		"2026-11-09": "2026-11-09", // Monday
		"2026-11-12": "2026-11-09",
		"2026-11-15": "2026-11-09", // Sunday stays in the week it ends
		"2026-11-16": "2026-11-16",
		"2026-01-01": "2025-12-29", // across a year
	} {
		d, _ := time.ParseInLocation(time.DateOnly, day, time.Local)
		if got := weekOf(d.Add(23 * time.Hour)); got != want {
			t.Errorf("weekOf(%s) = %s, want %s", day, got, want)
		}
	}
}

func TestRoadmap(t *testing.T) {
	kith, empty := t.TempDir(), t.TempDir()
	for _, f := range []string{"ROADMAP.md", "planning/roadmap.md", "notes.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(kith, f)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(kith, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := &Board{home: "/home", selfURL: "http://board"}
	var err error
	b.areas, err = b.parseAreas(areasOf(t, `[{"id": "work", "dir": "`+empty+`", "roadmap": []}, {"id": "kith", "dir": "`+kith+`"},
		{"id": "plans", "dir": "`+kith+`", "roadmap": ["*/roadmap.md", "ROADMAP.md", "planning/*.md", "["]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := b.areas[1].roadmapFiles(), []string{filepath.Join(kith, "ROADMAP.md")}; !slices.Equal(got, want) {
		t.Errorf("default roadmapFiles = %v, want %v", got, want)
	}
	// Each file once, in glob order; a bad glob matches nothing.
	plan := filepath.Join(kith, "planning", "roadmap.md")
	if got, want := b.areas[2].roadmapFiles(), []string{plan, filepath.Join(kith, "ROADMAP.md")}; !slices.Equal(got, want) {
		t.Errorf("roadmapFiles = %v, want %v", got, want)
	}
	if p, ids := b.roadmapPrompt([]string{"work", "plans"}); !strings.Contains(p, `area "plans": `+plan) || strings.Contains(p, `area "work"`) || !slices.Equal(ids, []string{"plans"}) {
		t.Errorf("roadmapPrompt lists the wrong files, for %v:\n%s", ids, p)
	}
	// Only the areas asked for.
	if p, ids := b.roadmapPrompt([]string{"kith"}); strings.Contains(p, `area "plans"`) || !slices.Equal(ids, []string{"kith"}) {
		t.Errorf("roadmapPrompt(kith) for %v:\n%s", ids, p)
	}
	if p, ids := b.roadmapPrompt([]string{"work"}); p != "" || ids != nil {
		t.Errorf("roadmapPrompt with no files = %q, %v; want none", p, ids)
	}
}

// areasOf decodes a JSON list of areas, as the settings page sends them.
func areasOf(t *testing.T, s string) []Area {
	t.Helper()
	var areas []Area
	if err := json.Unmarshal([]byte(s), &areas); err != nil {
		t.Fatal(err)
	}
	return areas
}

func TestAreas(t *testing.T) {
	work, kith := t.TempDir(), t.TempDir()
	// work's folder is a GitHub repository: its #numbers are its own pull requests, not HIRDMAN_PR_URL's.
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@github.com:o/work.git"}} {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		cmd.Env = slices.DeleteFunc(os.Environ(), func(e string) bool { return strings.HasPrefix(e, "GIT_") }) // a hook's GIT_DIR is this repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	b := &Board{home: "/home"}
	t.Setenv("HIRDMAN_BRIEF_EVERY", "6h") // an area without briefEvery's
	t.Setenv("HIRDMAN_PR_URL", "https://env/pull/")
	t.Setenv("HIRDMAN_ISSUE_URL", "")
	brief := []Run{{Name: "brief", Prompt: "Use the brief skill."}}
	areas, err := b.parseAreas(areasOf(t, `[{"id": "work", "name": "Work", "dir": "`+work+`", "runs": [{"name": "brief", "prompt": "Use the brief skill."}]},
		{"id": "kith", "name": "Kith", "dir": "`+kith+`", "briefEvery": "1h", "issueUrl": "https://kith/issues/", "prUrl": "https://kith/pull/"},
		{"id": "nixos", "dir": "~/dev/nixos", "runs": []}, {"id": "personal", "name": "Personal", "dir": "~", "briefEvery": "0"}]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Area{{"work", "Work", work, defaultRoadmap, brief, nil, nil, "", "", "https://github.com/o/work/pull/", Use{}, 6 * time.Hour},
		{"kith", "Kith", kith, defaultRoadmap, defaultRuns, nil, nil, "1h", "https://kith/issues/", "https://kith/pull/", Use{}, time.Hour},
		{"nixos", "nixos", "/home/dev/nixos", defaultRoadmap, []Run{}, nil, nil, "", "", "https://env/pull/", Use{}, 6 * time.Hour},
		{"personal", "Personal", "/home", defaultRoadmap, defaultRuns, nil, nil, "0", "", "https://env/pull/", Use{}, 0}}
	if !reflect.DeepEqual(areas, want) {
		t.Errorf("parseAreas = %v, want %v", areas, want)
	}
	// No area is required, work included.
	if got, err := b.parseAreas(nil); err != nil || len(got) != 0 {
		t.Errorf("parseAreas(none) = %v, %v; want none", got, err)
	}
	for _, bad := range []string{`[{"id": "Kith", "dir": "/"}]`, `[{"id": "work", "dir": "/"}, {"id": "work", "dir": "/"}]`,
		`[{"name": "no id", "dir": "/"}]`, `[{"id": "work"}]`, `[{"id": "work", "dir": "/", "briefEvery": "daily"}]`,
		`[{"id": "a", "dir": "/", "runs": [{"name": "empty"}]}]`, `[{"id": "a", "dir": "/", "runs": [{"builtin": "sweep"}]}]`,
		`[{"id": "a", "dir": "/", "runs": [{"prompt": "p", "builtin": "roadmap"}]}]`} {
		if _, err := b.parseAreas(areasOf(t, bad)); err == nil {
			t.Errorf("parseAreas(%s) took it", bad)
		}
	}
	b.areas = areas
	for area, want := range map[string]string{"work": work, "kith": kith} {
		if got, err := b.dirFor(&Task{Area: area}); got != want || err != nil {
			t.Errorf("dirFor(%q) = %s, %v; want %s", area, got, err, want)
		}
	}
	// No fallback: an unknown area or a missing folder is an error, not home.
	for _, area := range []string{"nixos", "gone", ""} {
		if got, err := b.dirFor(&Task{Area: area}); err == nil {
			t.Errorf("dirFor(%q) = %s, want an error", area, got)
		}
	}
	if b.badField(&Task{Area: "kith", Urgency: "medium", Importance: "medium"}) != "" || b.badField(&Task{Area: "gone"}) == "" || b.badField(&Task{}) == "" {
		t.Error("badField: want a configured area, required")
	}
	// A task saved before areas and levels were required loads as work, medium; the gone groups as levels and status:
	// "urgent" high urgency, "later" low, "waiting_on_others" an open task waiting (a closed one stays closed).
	b.path = filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(b.path, []byte(`{"tasks": {"old": {"id": "old"}, "k": {"id": "k", "area": "kith"},
		"u": {"id": "u", "group": "urgent"}, "ul": {"id": "ul", "group": "urgent", "urgency": "low"},
		"l": {"id": "l", "group": "later"}, "w": {"id": "w", "group": "waiting_on_others", "status": "todo"},
		"wd": {"id": "wd", "group": "waiting_on_others", "status": "done"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.load(); err != nil {
		t.Fatal(err)
	}
	if b.st.Tasks["old"].Area != "work" || b.st.Tasks["k"].Area != "kith" {
		t.Errorf("load: areas %q, %q; want work, kith", b.st.Tasks["old"].Area, b.st.Tasks["k"].Area)
	}
	for id, want := range map[string]string{"old": "medium", "u": "high", "ul": "low", "l": "low", "w": "medium"} {
		if got := b.st.Tasks[id]; got.Urgency != want || got.Importance != "medium" || got.Group != "" {
			t.Errorf("load %s: urgency %q, importance %q, group %q; want %s, medium, none", id, got.Urgency, got.Importance, got.Group, want)
		}
	}
	if w, wd := b.st.Tasks["w"], b.st.Tasks["wd"]; w.Status != "waiting" || wd.Status != "done" || wd.Group != "" {
		t.Errorf("load waiting_on_others: status %q, %q, group %q; want waiting, done, none", w.Status, wd.Status, wd.Group)
	}
}

func TestKithBack(t *testing.T) {
	dir := t.TempDir()
	lastOK := time.Now()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, wake: make(chan struct{}, 1),
		areas: []Area{{ID: "work"}, {ID: "personal"}}}
	b.st.Briefs = map[string]*Job{"work": {StartedAt: lastOK.Add(-time.Hour)}}
	if b.kithBack(lastOK) || len(b.rerun) != 0 {
		t.Error("a brief from before the outage was re-run")
	}
	b.st.Briefs["personal"] = &Job{StartedAt: lastOK.Add(time.Minute)} // ran while kith was down
	if !b.kithBack(lastOK) || !maps.Equal(b.rerun, map[string]bool{"personal": true}) || len(b.wake) != 1 {
		t.Errorf("re-run %v; want only the brief from during the outage", b.rerun)
	}
	b.kithBack(lastOK) // a second nudge must not block on the full channel
}

func TestHandleBriefRun(t *testing.T) {
	kith := t.TempDir()
	if err := os.WriteFile(filepath.Join(kith, "roadmap.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Board{wake: make(chan struct{}, 1), areas: []Area{{ID: "work", Runs: []Run{{Prompt: "brief"}}},
		{ID: "kith", Dir: kith, Roadmap: defaultRoadmap, Runs: defaultRuns}, {ID: "nixos", Dir: t.TempDir(), Roadmap: defaultRoadmap, Runs: defaultRuns}}}
	run := func(body string) int {
		rec := httptest.NewRecorder()
		b.handleBriefRun(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/api/brief/run", strings.NewReader(body)))
		return rec.Code
	}
	for body, want := range map[string]int{``: 400, `{"areas": []}`: 400, `{"areas": ["gone"]}`: 400, `{"areas": ["nixos"]}`: 409} {
		if code := run(body); code != want || len(b.rerun) != 0 {
			t.Errorf("%s: %d, rerun %v; want %d and no run", body, code, b.rerun, want)
		}
	}
	b.st.Briefs = map[string]*Job{"work": {Status: "running"}}
	if code := run(`{"areas": ["work"]}`); code != 409 || len(b.rerun) != 0 {
		t.Errorf("while running: %d, rerun %v; want 409 and no run", code, b.rerun)
	}
	// Only the areas asked for, and of them only those with a brief that isn't running.
	if code := run(`{"areas": ["work", "kith", "nixos"]}`); code != 202 || !maps.Equal(b.rerun, map[string]bool{"kith": true}) || len(b.wake) != 1 {
		t.Errorf("run: %d, rerun %v, wake %d; want 202 and kith nudged", code, b.rerun, len(b.wake))
	}
}

func TestDueBriefs(t *testing.T) {
	kith := t.TempDir()
	if err := os.WriteFile(filepath.Join(kith, "roadmap.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	prompt := []Run{{Prompt: "brief"}}
	b := &Board{areas: []Area{{ID: "work", Runs: prompt, every: 6 * time.Hour}, {ID: "personal", Runs: prompt, every: time.Hour},
		{ID: "kith", Dir: kith, Roadmap: defaultRoadmap, Runs: defaultRuns}, {ID: "nixos", Dir: t.TempDir(), Roadmap: defaultRoadmap, Runs: defaultRuns, every: time.Hour}}}
	b.st.Brief = map[string]any{"at": now.Add(-time.Hour).Format(time.RFC3339)} // the brief work pushed counts as its last
	b.st.Briefs = map[string]*Job{"personal": {StartedAt: now.Add(-2 * time.Hour)}}
	if got := b.dueBriefs(); !slices.Equal(got, []string{"personal"}) {
		t.Errorf("on schedule: %v, want personal", got)
	}
	// kith has no schedule but is named; nixos has nothing to brief; a running one's name waits for it to end.
	b.st.Briefs["personal"].Status = "running"
	b.briefNow("kith", "nixos", "personal")
	if got := b.dueBriefs(); !slices.Equal(got, []string{"kith"}) || !maps.Equal(b.rerun, map[string]bool{"personal": true}) {
		t.Errorf("named: %v, left %v; want kith, personal waiting", got, b.rerun)
	}
}

func TestMergePatch(t *testing.T) {
	cur := &Task{ID: "a", Title: "old", Status: "todo", JiraStatus: map[string]string{"X-1": "Open", "X-2": "Open"}}
	next, err := mergePatch(cur, map[string]json.RawMessage{
		"title":      json.RawMessage(`"new"`),
		"jiraStatus": json.RawMessage(`{"X-2": "Done"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Title != "new" || next.Status != "todo" {
		t.Errorf("fields: got title %q status %q", next.Title, next.Status)
	}
	if want := map[string]string{"X-1": "Open", "X-2": "Done"}; !maps.Equal(next.JiraStatus, want) {
		t.Errorf("jiraStatus = %v, want %v (merged per key)", next.JiraStatus, want)
	}
	if _, err := mergePatch(&Task{}, map[string]json.RawMessage{"jiraStatus": json.RawMessage(`{"X-1": "Done"}`)}); err != nil {
		t.Errorf("a task with no jiraStatus yet: %v", err)
	}
	if _, err := mergePatch(cur, map[string]json.RawMessage{"jiraStatus": json.RawMessage(`"Done"`)}); err == nil {
		t.Error("a jiraStatus that is not a map was accepted")
	}
}

func TestHandleUpsertLevels(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, areas: []Area{{ID: "work", Name: "Work", Dir: dir}}}
	b.st.Tasks = map[string]*Task{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/tasks/{id}", b.handleUpsert)
	put := func(body string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "PUT", "/api/tasks/t1", strings.NewReader(body)))
		return rec.Code
	}
	if code := put(`{"urgency": "high"}`); code != 400 {
		t.Fatalf("a new task with no area: %d, want 400", code)
	}
	if code := put(`{"area": "work", "urgency": "high", "importance": "low"}`); code != 200 {
		t.Fatalf("valid levels: %d", code)
	}
	if code := put(`{"urgency": "asap"}`); code != 400 || b.st.Tasks["t1"].Urgency != "high" {
		t.Errorf("unknown urgency: %d, urgency %q; want 400 and unchanged", code, b.st.Tasks["t1"].Urgency)
	}
	if code := put(`{"importance": ""}`); code != 200 || b.st.Tasks["t1"].Importance != "medium" {
		t.Errorf("clearing importance: %d, %q; want medium", code, b.st.Tasks["t1"].Importance)
	}
	for _, g := range []string{"urgent", "later", "waiting_on_others"} {
		if code := put(`{"group": "` + g + `"}`); code != 400 {
			t.Errorf("group %s: %d, want 400", g, code)
		}
	}
	for trig, want := range map[string]int{
		`{"on": "main", "do": "x"}`:                 200,
		`{"jiraStatus": "Done", "do": "x"}`:         200,
		`{"at": "2026-11-18T09:00:00Z", "do": "x"}`: 200,
		`{"do": "x"}`:    400,
		`{"on": "main"}`: 400,
		`{"on": "main", "at": "2026-11-18T09:00:00Z", "do": "x"}`: 400,
	} {
		if code := put(`{"triggers": [` + trig + `]}`); code != want {
			t.Errorf("trigger %s: %d, want %d", trig, code, want)
		}
	}
}

func TestUpsertSummaryHistory(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, areas: []Area{{ID: "work", Name: "Work", Dir: dir}}}
	b.st.Tasks = map[string]*Task{
		"t1": {ID: "t1", Area: "work", Summary: "old", Job: &Job{Action: "act", Status: "running"}},
		"t2": {ID: "t2", Area: "work", Summary: "old"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/tasks/{id}", b.handleUpsert)
	for _, id := range []string{"t1", "t2"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "PUT", "/api/tasks/"+id, strings.NewReader(`{"summary": "found it"}`)))
		if rec.Code != 200 {
			t.Fatalf("%s: %d", id, rec.Code)
		}
	}
	// A running session's story change goes into the history; the brief rewriting an idle task's does not.
	if h := b.st.Tasks["t1"].History; len(h) != 1 || h[0].Kind != "summary" || h[0].Text != "found it" {
		t.Errorf("t1 history %+v, want one summary entry", h)
	}
	if h := b.st.Tasks["t2"].History; len(h) != 0 {
		t.Errorf("t2 history %+v, want none", h)
	}
}

func TestSpawnSessionTerminal(t *testing.T) {
	dir := t.TempDir()
	// The terminal gets the session as its argv, run in the task's folder.
	b := &Board{selfURL: "http://board", terminal: []string{"sh", "-c", `printf '%s\n' "$PWD" "$@" > args.tmp && mv args.tmp args`, "sh"}}
	pane, err := b.spawnSession(defaultAgents["opus"], dir, "t'1", "s1", "--session-id", "u1", "do it")
	if err != nil || pane != "" {
		t.Fatalf("spawnSession = %q, %v; want no pane, no error", pane, err)
	}
	var out []byte
	for range 100 {
		if out, err = os.ReadFile(filepath.Join(dir, "args")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Its questions, API errors and exits go to the board: the hooks post them to the task, its id escaped out of the shell quotes.
	session := `[{"hooks":[{"command":"curl -s -X POST 'http://board/api/tasks/t%271/session' -H 'Content-Type: application/json' --data-binary @-","type":"command"}]}]`
	settings := `{"hooks":{"PreToolUse":[{"hooks":[{"command":"curl -s -X POST 'http://board/api/tasks/t%271/ask' -H 'Content-Type: application/json' --data-binary @-","timeout":86400,"type":"command"}],"matcher":"AskUserQuestion"}],` +
		`"SessionEnd":` + session + `,"Stop":` + session + `,"StopFailure":` + session + `}}`
	want := dir + "\nzsh\n-lic\nexec \"$0\" \"$@\"\nclaude\n--permission-mode\nauto\n--remote-control\ns1\n-n\ns1\n--settings\n" + settings + "\n--model\nopus\n--session-id\nu1\ndo it\n"
	if string(out) != want {
		t.Errorf("terminal got\n%s\nwant\n%s", out, want)
	}
}

func TestHandleAskAnswer(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.Tasks = map[string]*Task{"t1": {ID: "t1", Job: &Job{Action: "act", Status: "running"}}, "t2": {ID: "t2"}}
	srv := httptest.NewServer(b.routes())
	defer srv.Close()
	input := `{"questions":[{"question":"Color?","options":[{"label":"Red"},{"label":"Blue"}]},{"question":"Sizes?","multiSelect":true,"options":[{"label":"S"},{"label":"L"}]}]}`
	post := func(path, body string) (int, string) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		out, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(out)
	}
	// ask holds the hook's request until the board answers it, and returns what the hook printed.
	ask := func(id string) chan string {
		got := make(chan string, 1)
		go func() {
			_, out := post("/api/tasks/"+id+"/ask", `{"hook_event_name":"PreToolUse","tool_input":`+input+`}`)
			got <- out
		}()
		for range 200 {
			b.mu.Lock()
			waiting := b.asks[id] != nil
			b.mu.Unlock()
			if waiting {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return got
	}

	if code, _ := post("/api/tasks/t1/answer", `{"answers":{"Color?":"Red"}}`); code != 409 {
		t.Errorf("an answer with no question waiting: %d, want 409", code)
	}
	if code, out := post("/api/tasks/t2/ask", `{"tool_input":`+input+`}`); code != 204 || out != "" {
		t.Errorf("a question on a task with no session: %d %q, want 204 and nothing, so the terminal asks", code, out)
	}

	got := ask("t1")
	if string(b.st.Tasks["t1"].Job.Question) != input {
		t.Fatalf("question on the task = %s, want %s", b.st.Tasks["t1"].Job.Question, input)
	}
	if code, _ := post("/api/tasks/t1/answer", `{"answers":{"Color?":"Blue","Sizes?":"S, L"}}`); code != 200 {
		t.Fatalf("answer: %d", code)
	}
	var hook struct {
		Out struct {
			Decision string `json:"permissionDecision"`
			Input    struct {
				Questions []any             `json:"questions"`
				Answers   map[string]string `json:"answers"`
			} `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(<-got), &hook); err != nil || hook.Out.Decision != "allow" || len(hook.Out.Input.Questions) != 2 ||
		hook.Out.Input.Answers["Color?"] != "Blue" || hook.Out.Input.Answers["Sizes?"] != "S, L" {
		t.Errorf("hook output = %+v, %v; want allow with the questions and both answers", hook, err)
	}
	tk := b.st.Tasks["t1"]
	if h := tk.History[len(tk.History)-1]; tk.Job.Question != nil || h.Text != "Color? → Blue\nSizes? → S, L" {
		t.Errorf("after the answer: question %s, history %q", tk.Job.Question, h.Text)
	}

	got = ask("t1")
	if code, _ := post("/api/tasks/t1/answer", `{}`); code != 200 {
		t.Fatalf("hand back: %d", code)
	}
	if out := <-got; out != "" {
		t.Errorf("handed back to the terminal, the hook printed %q, want nothing", out)
	}
}

func TestSessionSayAndStop(t *testing.T) {
	dir := t.TempDir()
	// A fake wezterm: panes 7 and 9 are t1's and t3's sessions, pane 8 a pane that took t2's old id; every other call is logged.
	fake := "#!/bin/sh\nif [ \"$2\" = list ]; then echo '[{\"pane_id\":7,\"title\":\"✳ t1-act\"},{\"pane_id\":8,\"title\":\"zsh\"},{\"pane_id\":9,\"title\":\"✳ t3-act\"}]'; else echo \"$@\" >> " + dir + "/calls; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "wezterm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, areas: []Area{{ID: "work", Name: "Work", Dir: dir}}}
	b.st.Tasks = map[string]*Task{
		"t1": {ID: "t1", Area: "work", Status: "doing", Job: &Job{Action: "act", Status: "running", Session: "t1-act", Pane: "7"}},
		"t2": {ID: "t2", Area: "work", Status: "doing", Job: &Job{Action: "act", Status: "ok", Session: "t2-act", Pane: "8"}},
		"t3": {ID: "t3", Area: "work", Status: "doing", Job: &Job{Action: "act", Status: "running", Session: "t3-act", Pane: "9"}},
	}
	mux := b.routes()
	do := func(method, path, body string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
		return rec.Code
	}
	calls := func() string { out, _ := os.ReadFile(filepath.Join(dir, "calls")); return string(out) }

	if code := do(http.MethodPost, "/api/tasks/t1/act", `{"action":"act","comment":"-also check the tests"}`); code != 200 {
		t.Fatalf("act while the agent works: %d", code)
	}
	if want := "cli send-text --pane-id 7 -- -also check the tests\ncli send-text --pane-id 7 --no-paste \r\n"; calls() != want {
		t.Errorf("act while the agent works typed %q, want %q", calls(), want)
	}

	do(http.MethodPut, "/api/tasks/t1", `{"status":"done"}`)
	if strings.Contains(calls(), "kill-pane") {
		t.Error("a session closing its own task was stopped")
	}
	do(http.MethodPut, "/api/tasks/t1", `{"status":"doing"}`)
	do(http.MethodPut, "/api/tasks/t1?actor=eugene", `{"status":"done"}`)
	if !strings.HasSuffix(calls(), "cli kill-pane --pane-id 7\n") || b.st.Tasks["t1"].Job.Status != "stopped" {
		t.Errorf("closed on the board: calls %q, job %s; want pane 7 killed and the job stopped", calls(), b.st.Tasks["t1"].Job.Status)
	}
	do(http.MethodPost, "/api/tasks/t2/act", `{"action":"drop"}`)
	if strings.Contains(calls(), "--pane-id 8") {
		t.Error("dropping t2 killed pane 8, which is no longer its session")
	}
	if code := do(http.MethodDelete, "/api/tasks/t3", ""); code != 200 || !strings.HasSuffix(calls(), "cli kill-pane --pane-id 9\n") {
		t.Errorf("delete while the agent works: %d, calls %q; want 200 and pane 9 killed", code, calls())
	}
}

func TestActKeepsOneSession(t *testing.T) {
	dir := t.TempDir()
	// A fake wezterm: panes 7 and 9 are a's and d's open sessions, pane 8 a pane that took e's old id; spawn opens pane 42.
	fake := "#!/bin/sh\nif [ \"$2\" = list ]; then echo '[{\"pane_id\":7,\"title\":\"✳ a-act\"},{\"pane_id\":8,\"title\":\"zsh\"},{\"pane_id\":9,\"title\":\"✳ d-act\"}]'; exit; fi\n" +
		"echo \"$@\" >> " + dir + "/calls\nif [ \"$2\" = spawn ]; then echo 42; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "wezterm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, areas: []Area{{ID: "work", Name: "Work", Dir: dir}}}
	b.st.Tasks = map[string]*Task{
		"a": {ID: "a", Area: "work", Job: &Job{Action: "act", Status: "ok", Session: "a-act", Pane: "7", SessionID: "id-a", Rules: []string{"act"}}},
		"b": {ID: "b", Area: "work", Job: &Job{Action: "act", Status: "ok", Session: "b-act", SessionID: "id-b"}},
		"c": {ID: "c", Area: "work"},
		"d": {ID: "d", Area: "work", Job: &Job{Action: "act", Status: "ok", Session: "d-act", Pane: "9", SessionID: "id-d"}},
		"e": {ID: "e", Area: "work", Job: &Job{Action: "act", Status: "running", Session: "e-act", Pane: "8"}},
	}
	mux := b.routes()
	act := func(id, body string) (int, string) {
		_ = os.Remove(filepath.Join(dir, "calls"))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/tasks/"+id+"/act", strings.NewReader(body)))
		out, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return rec.Code, string(out)
	}

	// Its session is open: the new round is typed into it, just Eugene's message, as the session has act's rule already.
	if code, calls := act("a", `{"action":"act","comment":"go on"}`); code != 200 || !strings.HasPrefix(calls, "cli send-text --pane-id 7 -- Eugene pressed") ||
		!strings.Contains(calls, "guidance: go on") || strings.Contains(calls, "Task:") || strings.Contains(calls, "What to do") || strings.Contains(calls, "spawn") {
		t.Errorf("a: %d, calls %q; want only the message typed into pane 7", code, calls)
	}
	if j := b.st.Tasks["a"].Job; j.Status != "running" || j.Pane != "7" || j.SessionID != "id-a" {
		t.Errorf("a's job = %+v, want running in pane 7 on id-a", j)
	}
	// Its session closed: it is resumed, given act's rule, which it hasn't had, but not the task again.
	if code, calls := act("b", `{"action":"act"}`); code != 200 || !strings.Contains(calls, " --resume id-b ") ||
		!strings.Contains(calls, "What to do: "+actRule) || strings.Contains(calls, "Task:") {
		t.Errorf("b: %d, calls %q; want a spawn resuming id-b with the message and act's rule", code, calls)
	}
	if j := b.st.Tasks["b"].Job; j.Pane != "42" || j.SessionID != "id-b" || !slices.Equal(j.Rules, []string{"act"}) {
		t.Errorf("b's job = %+v, want pane 42 on id-b", j)
	}
	// No session yet: a new one, with an id of its own.
	if code, calls := act("c", `{"action":"act"}`); code != 200 || !strings.Contains(calls, " --session-id "+b.st.Tasks["c"].Job.SessionID+" ") || len(b.st.Tasks["c"].Job.SessionID) != 36 ||
		!strings.Contains(calls, "Task:") {
		t.Errorf("c: %d, calls %q, id %q; want a spawn with a new session id", code, calls, b.st.Tasks["c"].Job.SessionID)
	}
	// Fresh: the open session is stopped and a new one started.
	code, calls := act("d", `{"action":"act","fresh":true}`)
	if id := b.st.Tasks["d"].Job.SessionID; code != 200 || !strings.HasPrefix(calls, "cli kill-pane --pane-id 9\n") || id == "id-d" || !strings.Contains(calls, " --session-id "+id+" ") {
		t.Errorf("d: %d, calls %q; want pane 9 killed and a new session", code, calls)
	}
	// The agent works, but pane 8 is no longer its session: nothing to add to.
	if code, calls := act("e", `{"action":"act","comment":"x"}`); code != 409 || calls != "" {
		t.Errorf("e: %d, calls %q; want 409 and nothing typed", code, calls)
	}
}

// A Jira update's headless run ends with the task still holding its session, so Act goes on in it, not in a new one.
func TestUpdateKeepsSession(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.Tasks = map[string]*Task{"a": {ID: "a", Job: &Job{Action: "update", Status: "running", SessionID: "id-a"}}}
	b.run(Agent{Command: "true"}, "a", dir, "update", "x", &Job{Action: "act", Status: "ok", Session: "a-act", Pane: "7", SessionID: "id-a", Rules: []string{"act"}}, nil)
	if j := b.st.Tasks["a"].Job; j.Status == "running" || j.Session != "a-act" || j.Pane != "7" || j.SessionID != "id-a" || !slices.Equal(j.Rules, []string{"act"}) {
		t.Errorf("job after the update = %+v, want it ended, still on session a-act in pane 7 with id-a", j)
	}
}

// Closing a task with Jira keys runs the agent in its folder to move them on; one without keys runs nothing.
func TestCloseChecksJira(t *testing.T) {
	dir := t.TempDir()
	fake := "#!/bin/sh\npwd > " + dir + "/ran; printf '%s' \"$2\" > " + dir + "/prompt; for a; do m=$a; done; echo $m > " + dir + "/model\necho '{\"type\":\"result\",\"result\":\"BE-1: QA - Staging, moved\"}'\n"
	if err := os.WriteFile(filepath.Join(dir, "agent"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, areas: []Area{{ID: "work", Name: "Work", Dir: dir}},
		conf: Config{Agents: map[string]Agent{"haiku": {Command: filepath.Join(dir, "agent"), Model: "haiku"}}}}
	b.st.Tasks = map[string]*Task{
		"j":  {ID: "j", Title: "Fix it", Area: "work", Status: "waiting", Jira: []string{"BE-1"}},
		"no": {ID: "no", Area: "work", Status: "waiting"},
	}
	mux := b.routes()
	for _, id := range []string{"no", "j"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/tasks/"+id+"?actor=eugene", strings.NewReader(`{"status":"done"}`)))
		if rec.Code != 200 {
			t.Fatalf("close %s: %d", id, rec.Code)
		}
	}
	last := func() Entry {
		b.mu.Lock()
		defer b.mu.Unlock()
		h := b.st.Tasks["j"].History
		return h[len(h)-1]
	}
	for i := 0; i < 100 && last().Kind != "qa"; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if e := last(); e.Actor != "agent" || e.Text != "BE-1: QA - Staging, moved" {
		t.Errorf("last entry = %+v, want the agent's Jira report", e)
	}
	if ran, _ := os.ReadFile(filepath.Join(dir, "ran")); strings.TrimSpace(string(ran)) != dir {
		t.Errorf("agent ran in %q, want the area's folder %q", ran, dir)
	}
	if prompt, _ := os.ReadFile(filepath.Join(dir, "prompt")); !strings.Contains(string(prompt), "now: BE-1.") {
		t.Errorf("prompt %q doesn't name BE-1", prompt)
	}
	if model, _ := os.ReadFile(filepath.Join(dir, "model")); string(model) != "haiku\n" {
		t.Errorf("Jira check ran on model %q, want the light one, haiku", model)
	}
	if h := b.st.Tasks["no"].History; slices.ContainsFunc(h, func(e Entry) bool { return e.Kind == "qa" }) {
		t.Errorf("a task without Jira keys got a check: %+v", h)
	}
}

// Every task added gets a look-only agent run in its folder, whose findings go into its history; an update gets none.
func TestCreatePreps(t *testing.T) {
	dir := t.TempDir()
	fake := "#!/bin/sh\npwd >> " + dir + "/ran; printf '%s' \"$2\" > " + dir + "/prompt\necho '{\"type\":\"result\",\"result\":\"main.go:1 is it\"}'\n"
	if err := os.WriteFile(filepath.Join(dir, "agent"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, conf: Config{Agents: map[string]Agent{"opus": {Kind: "claude-code", Command: filepath.Join(dir, "agent")}}}, areas: []Area{{ID: "work", Name: "Work", Dir: dir}}}
	b.st.Tasks = map[string]*Task{}
	mux := b.routes()
	for _, path := range []string{"/api/tasks/brief", "/api/tasks/brief", "/api/tasks/mine?actor=eugene"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPut, path, strings.NewReader(`{"area":"work","title":"Look into X"}`)))
		if rec.Code != 200 {
			t.Fatalf("PUT %s: %d", path, rec.Code)
		}
	}
	prepped := func(id string) []Entry {
		b.mu.Lock()
		defer b.mu.Unlock()
		var es []Entry
		for _, e := range b.st.Tasks[id].History {
			if e.Kind == "prep" {
				es = append(es, e)
			}
		}
		return es
	}
	for i := 0; i < 100 && (len(prepped("brief")) == 0 || len(prepped("mine")) == 0); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	for _, id := range []string{"brief", "mine"} {
		if es := prepped(id); len(es) != 1 || es[0].Actor != "agent" || es[0].Text != "main.go:1 is it" {
			t.Errorf("%s: prep entries %+v, want one with the agent's findings", id, es)
		}
	}
	if ran, _ := os.ReadFile(filepath.Join(dir, "ran")); string(ran) != dir+"\n"+dir+"\n" {
		t.Errorf("agent ran %q, want twice, in the area's folder %q", ran, dir)
	}
	if prompt, _ := os.ReadFile(filepath.Join(dir, "prompt")); !strings.Contains(string(prompt), `"title":"Look into X"`) {
		t.Errorf("prompt %q doesn't carry the task", prompt)
	}
}

func TestSessionHook(t *testing.T) {
	dir := t.TempDir()
	// A fake wezterm: pane 7 is t1's session; every other call is logged.
	fake := "#!/bin/sh\nif [ \"$2\" = list ]; then echo '[{\"pane_id\":7,\"title\":\"✳ t1-act\"}]'; else echo \"$@\" >> " + dir + "/calls; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "wezterm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	retryDelay = 0
	t.Cleanup(func() { retryDelay = time.Minute })
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.Tasks = map[string]*Task{
		"t1": {ID: "t1", Job: &Job{Action: "act", Status: "running", Session: "t1-act", Pane: "7"}},
		"t2": {ID: "t2", Job: &Job{Action: "act", Status: "running", Session: "t2-act", Pane: "8"}},
	}
	mux := b.routes()
	hook := func(id, body string) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/tasks/"+id+"/session", strings.NewReader(body)))
		if rec.Code != http.StatusNoContent {
			t.Errorf("hook %s: %d", body, rec.Code)
		}
	}
	calls := func() string {
		b.mu.Lock() // continueSession writes under it
		defer b.mu.Unlock()
		out, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return string(out)
	}
	last := func(id string) string {
		b.mu.Lock()
		defer b.mu.Unlock()
		h := b.st.Tasks[id].History
		return h[len(h)-1].Text
	}

	// A turn that ends before the report shows on the board.
	hook("t1", `{"hook_event_name":"Stop","last_assistant_message":"Which branch should I use?"}`)
	if got := last("t1"); got != "Which branch should I use?" {
		t.Errorf("a stopped turn noted %q", got)
	}
	// A passing API error is continued, maxRetries times.
	for i := range maxRetries {
		hook("t1", `{"hook_event_name":"StopFailure","error":"overloaded","last_assistant_message":"API Error: 529"}`)
		want := strings.Repeat("cli send-text --pane-id 7 -- continue\ncli send-text --pane-id 7 --no-paste \r\n", i+1)
		for range 100 {
			if calls() == want {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if calls() != want {
			t.Fatalf("retry %d typed %q, want %q", i+1, calls(), want)
		}
	}
	hook("t1", `{"hook_event_name":"StopFailure","error":"overloaded","last_assistant_message":"API Error: 529"}`)
	if got := last("t1"); got != `Session "t1-act" stopped: API Error: 529. It waits for you in its terminal.` {
		t.Errorf("out of retries, noted %q", got)
	}
	// One a retry can't fix is only noted.
	hook("t2", `{"hook_event_name":"StopFailure","error":"authentication_failed","last_assistant_message":"Login expired · Please run /login"}`)
	if got := last("t2"); got != `Session "t2-act" stopped: Login expired · Please run /login. It waits for you in its terminal.` {
		t.Errorf("auth error noted %q", got)
	}
	// A session killed with its pane goes on running (so it can be resumed); Eugene exiting it ends the job.
	hook("t2", `{"hook_event_name":"SessionEnd","reason":"other"}`)
	if got := b.st.Tasks["t2"].Job.Status; got != "running" {
		t.Errorf("after SessionEnd other, job %q, want running", got)
	}
	hook("t2", `{"hook_event_name":"SessionEnd","reason":"prompt_input_exit"}`)
	if got := b.st.Tasks["t2"].Job.Status; got != "stopped" {
		t.Errorf("after /exit, job %q, want stopped", got)
	}
	// Once the job is over, the session's turns stay in its terminal.
	n := len(b.st.Tasks["t2"].History)
	hook("t2", `{"hook_event_name":"Stop","last_assistant_message":"Bye"}`)
	if len(b.st.Tasks["t2"].History) != n {
		t.Error("a turn after the job ended was noted")
	}
	if strings.Contains(calls(), "pane-id 8") {
		t.Error("typed into t2's pane")
	}
}

func TestHandleAttach(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.Tasks = map[string]*Task{"t1": {ID: "t1"}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks/{id}/files", b.handleAttach)
	post := func(id string, files map[string]string) int {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for name, content := range files {
			fw, err := mw.CreateFormFile("file", name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write([]byte(content))
		}
		_ = mw.Close()
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/tasks/"+id+"/files", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("t1", map[string]string{"a.txt": "one"}); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	if code := post("t1", map[string]string{"a.txt": "two", "../b.txt": "b"}); code != 200 {
		t.Fatalf("second upload: %d", code)
	}
	want := []string{filepath.Join(dir, "files", "t1", "a.txt"), filepath.Join(dir, "files", "t1", "b.txt")}
	if got := b.st.Tasks["t1"].Files; !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Errorf("Files = %v, want %v", got, want)
	}
	if data, _ := os.ReadFile(want[0]); string(data) != "two" {
		t.Errorf("a.txt = %q, want the replacement", data)
	}
	if code := post("nope", map[string]string{"a.txt": "x"}); code != 404 {
		t.Errorf("unknown task: %d, want 404", code)
	}
	if code := post("t1", map[string]string{"..": "x"}); code != 400 {
		t.Errorf("name ..: %d, want 400", code)
	}
}

func TestHandleDelete(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.Tasks = map[string]*Task{"t1": {ID: "t1"}, "busy": {ID: "busy", Job: &Job{Action: "act", Status: "running"}}}
	files := filepath.Join(dir, "files", "t1")
	if err := os.MkdirAll(files, 0o700); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/tasks/{id}", b.handleDelete)
	del := func(id string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "DELETE", "/api/tasks/"+id, nil))
		return rec.Code
	}
	if code := del("t1"); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if _, ok := b.st.Tasks["t1"]; ok {
		t.Error("t1 is still on the board")
	}
	if _, err := os.Stat(files); !os.IsNotExist(err) {
		t.Errorf("files/t1 still there: %v", err)
	}
	if code := del("t1"); code != 404 {
		t.Errorf("deleting again: %d, want 404", code)
	}
	if code := del("busy"); code != 409 || b.st.Tasks["busy"] == nil {
		t.Errorf("a task the agent works on: %d, want 409 and kept", code)
	}
}

func TestReapSessions(t *testing.T) {
	// The mux lists panes 7, 8 and 9, where 9 took an old session's id; a spawn opens pane 21 and logs its argv.
	bin := t.TempDir()
	list := `[{"pane_id":7,"title":"✳ s1"},{"pane_id":8,"title":"✳ s5"},{"pane_id":9,"title":"zsh"}]`
	fake := "#!/bin/sh\nif [ \"$2\" = spawn ]; then echo \"$@\" >> " + bin + "/spawned; echo 21; else echo '" + list + "'; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "wezterm"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, areas: []Area{{ID: "work", Name: "Work", Dir: dir}}}
	b.st.Tasks = map[string]*Task{
		"live":    {ID: "live", Area: "work", Job: &Job{Action: "act", Status: "running", Session: "s1", Pane: "7"}},
		"dead":    {ID: "dead", Area: "work", Job: &Job{Action: "act", Status: "running", Session: "s2", Pane: "17"}},
		"resumed": {ID: "resumed", Area: "work", Job: &Job{Action: "act", Status: "running", Session: "s3", SessionID: "u3", Pane: "18"}},
		"retried": {ID: "retried", Area: "work", Job: &Job{Action: "act", Status: "running", Session: "s4", SessionID: "u4", Pane: "19", Retries: maxRetries}},
		"open":    {ID: "open", Area: "work", Job: &Job{Action: "act", Status: "ok", Session: "s5", Pane: "8"}},
		"shut":    {ID: "shut", Area: "work", Job: &Job{Action: "act", Status: "ok", Session: "s6", Pane: "28"}},
		"took":    {ID: "took", Area: "work", Job: &Job{Action: "act", Status: "ok", Session: "s7", Pane: "9"}},
	}
	b.reapSessions()
	if got := b.st.Tasks["live"].Job.Status; got != "running" {
		t.Errorf("live pane's job = %q, want running", got)
	}
	// Without a session id, or out of retries, a gone pane's job fails; with one, its session is resumed by id.
	for _, id := range []string{"dead", "retried"} {
		if got := b.st.Tasks[id].Job.Status; got != "failed" {
			t.Errorf("%s: gone pane's job = %q, want failed", id, got)
		}
	}
	if j := b.st.Tasks["resumed"].Job; j.Status != "running" || j.Pane != "21" || j.Retries != 1 {
		t.Errorf("resumed job = %+v, want running in pane 21, 1 retry", j)
	}
	if out, _ := os.ReadFile(filepath.Join(bin, "spawned")); !strings.Contains(string(out), "--resume u3 "+resumePrompt) || strings.Count(string(out), "spawn") != 1 {
		t.Errorf("spawned %q, want one session resuming u3", out)
	}
	// An ended job keeps its pane while its session is open there, and forgets it once the session closed.
	for id, want := range map[string]string{"open": "8", "shut": "", "took": ""} {
		if got := b.st.Tasks[id].Job.Pane; got != want {
			t.Errorf("%s's pane = %q, want %q", id, got, want)
		}
	}
	if _, err := os.Stat(b.path); err != nil {
		t.Errorf("reaping did not save: %v", err)
	}
}

func TestGhTasks(t *testing.T) {
	b := &Board{st: State{Tasks: map[string]*Task{
		"pr":     {ID: "pr", Links: []map[string]string{{"url": "https://github.com/o/r/pull/7#discussion_r1"}}},
		"jira":   {ID: "jira", Jira: []string{"TM-2"}},
		"pr70":   {ID: "pr70", Links: []map[string]string{{"url": "https://github.com/o/r/pull/70"}}},
		"other":  {ID: "other", Jira: []string{"TM-20"}},
		"issue7": {ID: "issue7", Links: []map[string]string{{"url": "https://github.com/o/r/issues/7"}}},
	}}}
	n := &ghNotification{}
	n.Subject.URL = "https://api.github.com/repos/o/r/pulls/7"
	n.Subject.Title = "TM-2: fix it"
	var ids []string
	for _, t := range b.ghTasks(n, regexp.MustCompile(`\b[A-Z][A-Z0-9]+-\d+\b`)) {
		ids = append(ids, t.ID)
	}
	slices.Sort(ids)
	if want := []string{"jira", "pr"}; !slices.Equal(ids, want) {
		t.Errorf("ghTasks = %v, want %v", ids, want)
	}
	// The key brought pull request 7 to "jira", so it is jira's now; "pr" linked it already.
	if got := taskPulls(b.st.Tasks["jira"]); !slices.Equal(got, []string{"https://github.com/o/r/pull/7"}) || len(b.st.Tasks["pr"].Links) != 1 {
		t.Errorf("jira's pulls = %v, pr's links = %v", got, b.st.Tasks["pr"].Links)
	}
}

func TestGhStatus(t *testing.T) {
	status, etag, err := ghStatus([]byte("HTTP/2.0 304 Not Modified\r\nEtag: W/\"abc\"\r\nX-Poll-Interval: 60\r\n\r\n"))
	if err != nil || status != http.StatusNotModified || etag != `W/"abc"` {
		t.Errorf("ghStatus = %d %q %v, want 304 W/\"abc\"", status, etag, err)
	}
}

func TestTaskPulls(t *testing.T) {
	task := &Task{Links: []map[string]string{
		{"url": "https://github.com/o/r/pull/7#discussion_r1"},
		{"url": "https://github.com/o/r/pull/7/files"},
		{"url": "https://github.com/o/r/issues/8"},
		{"url": "https://github.com/o/s/pull/9"},
	}}
	if got, want := taskPulls(task), []string{"https://github.com/o/r/pull/7", "https://github.com/o/s/pull/9"}; !slices.Equal(got, want) {
		t.Errorf("taskPulls = %v, want %v", got, want)
	}
}

func TestLinkPulls(t *testing.T) {
	task := &Task{Links: []map[string]string{{"label": "PR #7", "url": "https://github.com/o/r/pull/7#discussion_r1"}}}
	linkPulls(task, "Opened #9 https://github.com/o/r/pull/9, review on https://github.com/o/r/pull/7/files and https://github.com/o/r/pull/9.")
	want := []map[string]string{task.Links[0], {"label": "PR #9", "url": "https://github.com/o/r/pull/9"}}
	if !reflect.DeepEqual(task.Links, want) {
		t.Errorf("links = %v, want %v", task.Links, want)
	}
}

func TestEndsWithPulls(t *testing.T) {
	for w, want := range map[string]bool{
		"": true, "PR review": true, "PR #27 review": true, "pull request merge": true,
		"review of #618 (part 1) and stacked #620 (WhatsApp media retry)": true, "merge of #12": true,
		"prod deploy of release #1034, then re-time a finished match": false,
		"Bar/Polina: OK to pay + #1024 re-review":                     false,
	} {
		if got := endsWithPulls(w); got != want {
			t.Errorf("endsWithPulls(%q) = %v, want %v", w, got, want)
		}
	}
}

func TestPullsDone(t *testing.T) {
	merged, closed, open := ghPull{State: "closed", Merged: true}, ghPull{State: "closed"}, ghPull{State: "open"}
	for _, c := range []struct {
		pulls []ghPull
		want  bool
	}{
		{[]ghPull{merged}, true},
		{[]ghPull{merged, closed}, true},
		{[]ghPull{merged, open}, false},
		{[]ghPull{closed}, false},
		{nil, false},
	} {
		if got := pullsDone(c.pulls); got != c.want {
			t.Errorf("pullsDone(%v) = %v, want %v", c.pulls, got, c.want)
		}
	}
}

func TestPullsShipped(t *testing.T) {
	onMain, onDev, closed := ghPull{Merged: true, Shipped: true}, ghPull{Merged: true}, ghPull{State: "closed"}
	for _, c := range []struct {
		pulls []ghPull
		want  bool
	}{
		{[]ghPull{onMain, closed}, true},
		{[]ghPull{onMain, onDev}, false},
	} {
		if got := pullsShipped(c.pulls); got != c.want {
			t.Errorf("pullsShipped(%v) = %v, want %v", c.pulls, got, c.want)
		}
	}
}

func TestReleased(t *testing.T) {
	p := ghPull{MergedAt: "2026-11-16T18:10:27Z"}
	before, after, open := ghPull{MergedAt: "2026-11-16T17:00:00Z"}, ghPull{MergedAt: "2026-11-16T18:48:30Z"}, ghPull{}
	for _, c := range []struct {
		releases []ghPull
		want     bool
	}{
		{[]ghPull{before, open}, false},
		{[]ghPull{before, after}, true},
		{nil, false},
	} {
		if got := released(&p, c.releases); got != c.want {
			t.Errorf("released(%v) = %v, want %v", c.releases, got, c.want)
		}
	}
}

// gitRepo is a fresh repo with one commit, and tree, which makes .worktrees/name on a new branch name
// with one commit and returns its path and tip (the merged head).
func gitRepo(t *testing.T) (repo string, run func(dir string, args ...string) string, tree func(name string) (string, string)) {
	t.Helper()
	// Run as a git hook (pre-push), GIT_DIR and friends would point every git call at the real repo.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			t.Setenv(k, "") // restores it after the test
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "t")
		t.Setenv(k+"_EMAIL", "t@t")
	}
	run = func(dir string, args ...string) string {
		t.Helper()
		out, err := git(dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	repo = t.TempDir()
	run(repo, "init", "-q", "-b", "main")
	run(repo, "commit", "-q", "--allow-empty", "-m", "init")
	tree = func(name string) (string, string) {
		path := filepath.Join(repo, ".worktrees", name)
		run(repo, "worktree", "add", "-q", path, "-b", name)
		run(path, "commit", "-q", "--allow-empty", "-m", name)
		return path, run(path, "rev-parse", "HEAD")
	}
	return repo, run, tree
}

func TestPruneWorktree(t *testing.T) {
	repo, run, tree := gitRepo(t)

	path, sha := tree("merged")
	if got, err := pruneWorktree(repo, "merged", sha); err != nil || got != "removed .worktrees/merged and branch merged, merged" {
		t.Errorf("merged: %q, %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("merged worktree still there: %v", err)
	}
	if _, err := git(repo, "rev-parse", "--verify", "refs/heads/merged"); err == nil {
		t.Error("merged branch still there")
	}

	path, sha = tree("dirty")
	if err := os.WriteFile(filepath.Join(path, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ahead, aheadSHA := tree("ahead")
	run(ahead, "commit", "-q", "--allow-empty", "-m", "after the merge")
	locked, lockedSHA := tree("locked")
	run(repo, "worktree", "lock", locked)
	busy, busySHA := tree("busy")
	sleep := exec.CommandContext(t.Context(), "sleep", "30")
	sleep.Dir = busy
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleep.Process.Kill(); _ = sleep.Wait() }()

	for _, c := range []struct{ branch, sha, want string }{
		{"dirty", sha, "kept .worktrees/dirty: it has uncommitted changes"},
		{"ahead", aheadSHA, "kept .worktrees/ahead: ahead has moved on from the merged head"},
		{"locked", lockedSHA, "kept .worktrees/locked: it is locked"},
		{"busy", busySHA, "kept .worktrees/busy: a process (a session?) is running in it"},
		{"main", "", ""}, // the main checkout is never one
		{"nowhere", "", ""},
	} {
		if got, err := pruneWorktree(repo, c.branch, c.sha); err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.branch, got, err, c.want)
		}
	}
	for _, p := range []string{path, ahead, locked, busy} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("kept worktree gone: %v", err)
		}
	}
}

func TestCleanMerged(t *testing.T) {
	repo, _, tree := gitRepo(t)
	path, sha := tree("feature")
	// The session's shell sits in the worktree until its pane is killed.
	shell := exec.CommandContext(t.Context(), "sleep", "30")
	shell.Dir = path
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = shell.Wait() }()
	defer func() { _ = shell.Process.Kill() }()

	bin, dir := t.TempDir(), t.TempDir()
	killed := filepath.Join(dir, "killed")
	for name, script := range map[string]string{
		"gh":      `echo '{"state":"closed","merged":true,"head":{"ref":"feature","sha":"` + sha + `"}}'`,
		"wezterm": `case "$2" in list) echo '[{"pane_id":7}]';; kill-pane) echo "$@" >` + killed + `; kill ` + strconv.Itoa(shell.Process.Pid) + `;; esac`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.Tasks = map[string]*Task{"t": {ID: "t", Job: &Job{Status: "ok", Pane: "7"}}}

	b.cleanMerged("t", repo, "7", []string{"https://github.com/o/r/pull/1"})
	if got, _ := os.ReadFile(killed); string(got) != "cli kill-pane --pane-id 7\n" {
		t.Errorf("wezterm got %q, want the pane killed", got)
	}
	var notes []string
	for _, e := range b.st.Tasks["t"].History {
		notes = append(notes, e.Text)
	}
	if want := []string{
		"closed its session (wezterm pane 7): its pull request is merged",
		"removed .worktrees/feature and branch feature, merged",
	}; !slices.Equal(notes, want) {
		t.Errorf("notes = %q, want %q", notes, want)
	}
}

func TestPollOwn(t *testing.T) {
	// The gh user merged pull request 1 after the last poll and pull request 2 before it; GitHub notified nobody.
	bin, dir := t.TempDir(), t.TempDir()
	gh := `case "$*" in
*.login\ user) echo me;;
-i*) printf 'HTTP/1.1 200 OK\r\nEtag: "e1"\r\n\r\n';;
users/me/events*) echo '[
 {"type":"PullRequestEvent","created_at":"2026-11-17T16:00:00Z","payload":{"action":"merged","pull_request":{"url":"https://api.github.com/repos/o/r/pulls/1"}}},
 {"type":"PullRequestEvent","created_at":"2026-11-17T09:00:00Z","payload":{"action":"merged","pull_request":{"url":"https://api.github.com/repos/o/r/pulls/2"}}}]';;
*/pulls/1) echo '{"state":"closed","merged":true,"closed_at":"2026-11-17T16:00:00Z","base":{"ref":"main","repo":{"default_branch":"main"}}}';;
*/pulls/2) echo '{"state":"closed","merged":true,"closed_at":"2026-11-17T09:00:00Z","base":{"ref":"main","repo":{"default_branch":"main"}}}';;
*) exit 1;;
esac`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nshift\n"+gh+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.GitHubOwnSince = "2026-11-17T12:00:00Z"
	b.st.Tasks = map[string]*Task{
		"new": {ID: "new", Status: "waiting", WaitingFor: "PR review", Links: []map[string]string{{"url": "https://github.com/o/r/pull/1"}}},
		"old": {ID: "old", Status: "waiting", WaitingFor: "PR review", Links: []map[string]string{{"url": "https://github.com/o/r/pull/2"}}},
	}
	if err := b.pollOwn(); err != nil {
		t.Fatal(err)
	}
	if got := b.st.Tasks["new"].Status; got != "done" {
		t.Errorf("new: %s, want done", got)
	}
	if got := b.st.Tasks["old"].Status; got != "waiting" {
		t.Errorf("old: %s, want waiting (merged before the last poll)", got)
	}
	if b.st.GitHubOwnSince != "2026-11-17T16:00:00Z" || b.ghOwn != `"e1"` {
		t.Errorf("since %s, etag %s; want the newest event's and its ETag", b.st.GitHubOwnSince, b.ghOwn)
	}
}

func TestFullNames(t *testing.T) {
	b := &Board{}
	b.st.Tasks = map[string]*Task{"a": {Who: "Polina Shapiro"}, "b": {Who: "Bar Worcel"}, "c": {Who: "Sagi Chizik, Sagi Levi"}}
	for in, want := range map[string]string{
		"Polina":                  "Polina Shapiro",
		"polina":                  "Polina Shapiro",
		"Bar + Polina":            "Bar Worcel, Polina Shapiro",
		"Bar/Polina Shapiro":      "Bar Worcel, Polina Shapiro",
		"Bar and Polina, Bar":     "Bar Worcel, Polina Shapiro",
		"Evgeny":                  "Evgeny",
		"Evgeny Subotin & Evgeny": "Evgeny Subotin",
		"Sagi":                    "Sagi",
		"  ":                      "",
	} {
		task := &Task{Who: in}
		if b.fullNames(task); task.Who != want {
			t.Errorf("%q: %q, want %q", in, task.Who, want)
		}
	}
}

func TestFireTriggers(t *testing.T) {
	// Pull request 1 merged into development; release 9 into main came after it and carries its merge commit, release 8 came before.
	bin, dir := t.TempDir(), t.TempDir()
	gh := `case "$1" in
*/pulls/1) echo '{"number":1,"merged":true,"merged_at":"2026-11-16T18:00:00Z","merge_commit_sha":"abc","base":{"ref":"development"}}';;
*/pulls\?*base=main) echo '[{"number":9,"merged_at":"2026-11-16T19:00:00Z"},{"number":8,"merged_at":"2026-11-16T17:00:00Z"}]';;
*/pulls\?*) echo '[]';;
*/pulls/9/commits*) echo '[{"sha":"abc"}]';;
*/pulls/8/commits*) echo '[{"sha":"old"}]';;
*) exit 1;;
esac`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nshift\n"+gh+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for branch, want := range map[string]bool{"development": true, "main": true, "staging": false} {
		if got, err := onBranch("https://github.com/o/r/pull/1", branch); got != want || err != nil {
			t.Errorf("onBranch(%s) = %v, %v; want %v", branch, got, err, want)
		}
	}

	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, terminal: []string{"true"},
		areas: []Area{{ID: "work", Dir: dir}}}
	pr := []map[string]string{{"url": "https://github.com/o/r/pull/1"}}
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	b.st.Tasks = map[string]*Task{
		"dev":     {ID: "dev", Area: "work", Status: "done", Links: pr, Triggers: []Trigger{{On: "development", Do: "tell Bar"}, {On: "staging", Do: "later"}}},
		"dropped": {ID: "dropped", Area: "work", Status: "dropped", Links: pr, Triggers: []Trigger{{On: "main", Do: "no"}}},
		"busy":    {ID: "busy", Area: "work", Links: pr, Triggers: []Trigger{{On: "main", Do: "wait"}}, Job: &Job{Status: "running"}},
		// Neither needs GitHub: a time that came and one that hasn't, a Jira status reached and one that isn't.
		"at":   {ID: "at", Area: "work", Triggers: []Trigger{{At: &past, Do: "ping"}, {At: &future, Do: "not yet"}}},
		"jira": {ID: "jira", Area: "work", Jira: []string{"BE-1", "BE-2"}, JiraStatus: map[string]string{"BE-2": "QA - Staging"}, Triggers: []Trigger{{JiraStatus: "Done", Do: "no"}, {JiraStatus: "qa - staging", Do: "tell QA"}}},
	}
	b.fireTriggers(false)
	if b.st.Tasks["dev"].Triggers[0].Fired != nil {
		t.Error("a branch trigger fired without GitHub")
	}
	if at := b.st.Tasks["at"]; at.Triggers[0].Fired == nil || at.Triggers[1].Fired != nil || at.History[0].Actor != "board" {
		t.Errorf("at = %+v, want only the past time fired, by the board", at)
	}
	if j := b.st.Tasks["jira"]; j.Triggers[0].Fired != nil || j.Triggers[1].Fired == nil || j.History[0].Text != "BE-2 reached QA - Staging; do: tell QA" {
		t.Errorf("jira = %+v, want the QA trigger fired", j)
	}
	b.fireTriggers(true)
	dev := b.st.Tasks["dev"]
	if dev.Triggers[0].Fired == nil || dev.Triggers[1].Fired != nil {
		t.Errorf("dev triggers = %+v, want only development fired", dev.Triggers)
	}
	if dev.Job == nil || dev.Job.Action != "trigger" || dev.Job.Status != "running" {
		t.Errorf("dev job = %+v, want a running trigger session", dev.Job)
	}
	if e := dev.History[0]; e.Kind != "trigger" || e.Text != "a pull request it links reached development; do: tell Bar" {
		t.Errorf("dev note = %+v", e)
	}
	if b.st.Tasks["dropped"].Triggers[0].Fired != nil || b.st.Tasks["busy"].Triggers[0].Fired != nil {
		t.Error("a dropped or busy task's trigger fired")
	}
}

func TestSaveSettings(t *testing.T) {
	dir := t.TempDir()
	for k := range (&Config{}).settings() { // not the live service's env
		t.Setenv(k, "")
	}
	conf := filepath.Join(dir, "hirdman", "config.toml")
	b := &Board{path: filepath.Join(dir, "state.json"), confPath: conf, triage: make(chan struct{}, 1), restart: make(chan struct{}, 1),
		conf: Config{PRURL: "https://file/pull/", Areas: []Area{{ID: "work", Dir: dir, Runs: []Run{{Name: "brief", Prompt: "Use the brief skill.\nSend nothing."}}}}}}
	save := func(body string) int {
		rec := httptest.NewRecorder()
		b.handleSaveSettings(rec, httptest.NewRequestWithContext(t.Context(), "PUT", "/api/settings", strings.NewReader(body)))
		return rec.Code
	}
	for _, body := range []string{`{"HIRDMAN_JIRA_POLL": "soon"}`, `{"HIRDMAN_ADDR": ":1"}`, `{"HIRDMAN_AREAS": "{}"}`,
		`{"HIRDMAN_AREAS": "[{\"id\": \"Work\", \"dir\": \"/\"}]"}`, `{"HIRDMAN_ISSUE_KEY": "("}`, `{"HIRDMAN_SESSION_IN": "pane"}`} {
		if code := save(body); code != 400 {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	b.st.Briefs = map[string]*Job{"work": {Status: "running"}}
	if code := save(`{"HIRDMAN_GITHUB_POLL": "5m"}`); code != 409 {
		t.Errorf("while a brief runs: %d, want 409", code)
	}
	if _, err := os.Stat(conf); !os.IsNotExist(err) || len(b.restart) != 0 {
		t.Fatalf("a refused save wrote config.toml (%v) or restarted (%d)", err, len(b.restart))
	}
	b.st.Briefs = nil
	// The areas and settings sent go over the file's; an emptied one leaves it.
	if code := save(`{"HIRDMAN_GITHUB_POLL": " 5m ", "HIRDMAN_PR_URL": "", "HIRDMAN_AREAS": "[{\"id\": \"kith\", \"dir\": \"~/dev/kith\", \"roadmap\": []}]"}`); code != 202 || len(b.restart) != 1 {
		t.Fatalf("save: %d, restart %d; want 202 and a restart", code, len(b.restart))
	}
	got, err := loadConfig(conf)
	if want := (Config{GitHubPoll: "5m", Areas: []Area{{ID: "kith", Dir: "~/dev/kith", Roadmap: []string{}}}}); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("config.toml read back %+v (%v), want %+v", got, err, want)
	}
	// Multi-line prompts, runs = [] (no brief), mcp = [] (no server), [mcp] and feeds survive the round trip.
	b.conf.Areas[0].Runs = append(b.conf.Areas[0].Runs, Run{Builtin: "roadmap"})
	b.conf.Areas = append(b.conf.Areas, Area{ID: "nixos", Dir: dir, Runs: []Run{}, MCP: []string{},
		Feeds: []Feed{{MCP: "kith", Tool: "read_room", Args: map[string]any{"room": "r", "limit": int64(50)}}}})
	b.conf.MCP = map[string]MCPServer{"kith": {Command: "kith-mcp", Args: []string{"-v"}}, "atlassian": {URL: "https://mcp.example/mcp"}}
	if code := save(`{}`); code != 202 {
		t.Fatalf("save: %d", code)
	}
	if got, err := loadConfig(conf); err != nil || !reflect.DeepEqual(got, b.conf) {
		t.Errorf("config.toml read back %+v (%v), want %+v", got, err, b.conf)
	}

	// The env wins over the file; no file is an empty config; an unknown key is refused.
	t.Setenv("HIRDMAN_PR_URL", "https://env/pull/")
	if before := applyConfig(&b.conf); before["HIRDMAN_GITHUB_POLL"] != "" || os.Getenv("HIRDMAN_PR_URL") != "https://env/pull/" {
		t.Errorf("before %v, HIRDMAN_PR_URL %q; want the env's", before["HIRDMAN_GITHUB_POLL"], os.Getenv("HIRDMAN_PR_URL"))
	}
	b.conf.GitHubPoll = "5m"
	applyConfig(&b.conf)
	if os.Getenv("HIRDMAN_GITHUB_POLL") != "5m" {
		t.Errorf("HIRDMAN_GITHUB_POLL %q, want the file's 5m", os.Getenv("HIRDMAN_GITHUB_POLL"))
	}
	if c, err := loadConfig(filepath.Join(dir, "none.toml")); err != nil || !reflect.DeepEqual(c, Config{}) {
		t.Errorf("no file: %+v, %v; want an empty config", c, err)
	}
	if err := os.WriteFile(conf, []byte("githubpol = \"5m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(conf); err == nil {
		t.Error("an unknown key was taken")
	}
}

// Each due area's prompts run in its folder, in order; the roadmap areas share one run from home.
func TestRunBriefs(t *testing.T) {
	dir, work, kith := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(kith, "roadmap.md"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "agent")
	fake := "#!/bin/sh\nfor a; do m=$a; done; printf '%s %.20s %s\\n' \"$PWD\" \"$2\" \"$m\" >> " + dir + "/calls\necho '{\"type\":\"result\",\"result\":\"done\"}'\n"
	if err := os.WriteFile(agent, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := Config{Agents: map[string]Agent{"opus": {Command: agent, Model: "opus"}, "haiku": {Command: agent, Model: "haiku"}}}
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, conf: conf, home: dir, areas: []Area{
		{ID: "work", Dir: work, Runs: []Run{{Name: "brief", Prompt: "Use the brief skill.", Agent: "haiku"}, {Builtin: "roadmap"}}},
		{ID: "kith", Dir: kith, Roadmap: defaultRoadmap, Runs: defaultRuns},
		{ID: "personal", Dir: dir, Runs: []Run{{Name: "sweep", Prompt: "Use the sweep skill."}}}}}
	now := time.Now()
	b.st.Briefs = map[string]*Job{"work": {StartedAt: now}, "kith": {StartedAt: now}, "personal": {StartedAt: now}}
	b.runBriefs([]string{"work", "kith"})
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	// The work brief runs on its own agent, the roadmap run on the board's headless one.
	if want := work + " Use the brief skill. haiku\n" + dir + " Keep Eugene's Hirdma opus\n"; string(calls) != want {
		t.Errorf("runs:\n%s\nwant:\n%s", calls, want)
	}
	if b.st.Briefs["work"].Status != "ok" || b.st.Briefs["kith"].Status != "ok" || b.st.Briefs["personal"].EndedAt != nil {
		t.Errorf("briefs %+v %+v %+v; want work and kith ended ok, personal untouched", *b.st.Briefs["work"], *b.st.Briefs["kith"], *b.st.Briefs["personal"])
	}
}

func TestPromptSendsOnlyItsRule(t *testing.T) {
	b := &Board{selfURL: "http://board"}
	for action, rule := range actionRules {
		p, err := b.prompt(&Task{ID: "x", Title: "T"}, actRequest{Action: action})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p, "What to do: "+rule) || !strings.Contains(p, `{"id":"x","title":"T"`) {
			t.Errorf("prompt(%s) misses its rule or the compact task:\n%s", action, p)
		}
		for other, r := range actionRules {
			if other != action && !strings.Contains(rule, r) && strings.Contains(p, r) {
				t.Errorf("prompt(%s) carries the %s rule", action, other)
			}
		}
	}
	if !strings.Contains(actionRules["trigger"], actRule) {
		t.Error("a trigger's session lacks act's rules")
	}
}

func TestHandleCombine(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	day := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	gonePath := filepath.Join(dir, "files", "new", "a.txt")
	b.st.Tasks = map[string]*Task{
		"old": {ID: "old", Title: "Old", Summary: "first", Tags: []string{"x"}, Who: "Bar Levi",
			Files:   []string{filepath.Join(dir, "files", "old", "a.txt")},
			History: []Entry{{At: day, Kind: "created"}, {At: day.Add(2 * time.Hour), Kind: "note"}}},
		"new": {ID: "new", Title: "New", Summary: "second", Tags: []string{"x", "y"}, Who: "Dana Cohen", Jira: []string{"BE-1"},
			Files:   []string{gonePath},
			History: []Entry{{At: day.Add(time.Hour), Kind: "created"}}},
		"busy": {ID: "busy", History: []Entry{{At: day.Add(3 * time.Hour)}}, Job: &Job{Action: "act", Status: "running"}},
	}
	for _, f := range []string{b.st.Tasks["old"].Files[0], gonePath} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks/{id}/combine", b.handleCombine)
	combine := func(id, with string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "POST", "/api/tasks/"+id+"/combine", strings.NewReader(`{"with":"`+with+`"}`)))
		return rec.Code
	}
	// Pressed on the newer task: the older one still stays.
	if code := combine("new", "old"); code != 200 {
		t.Fatalf("combine: %d", code)
	}
	if _, ok := b.st.Tasks["new"]; ok {
		t.Error("the newer task is still on the board")
	}
	got := b.st.Tasks["old"]
	moved := filepath.Join(dir, "files", "old", "new-a.txt")
	if data, err := os.ReadFile(moved); err != nil || string(data) != gonePath {
		t.Errorf("moved file: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Dir(gonePath)); !os.IsNotExist(err) {
		t.Errorf("files/new still there: %v", err)
	}
	kinds := make([]string, 0, len(got.History))
	for _, e := range got.History {
		kinds = append(kinds, e.Kind)
	}
	if want := []string{"created", "created", "note", "combine"}; !slices.Equal(kinds, want) {
		t.Errorf("history kinds = %v, want %v", kinds, want)
	}
	if got.Summary != "first\n\n**New**: second" || !slices.Equal(got.Tags, []string{"x", "y"}) || !slices.Equal(got.Jira, []string{"BE-1"}) ||
		got.Who != "Bar Levi, Dana Cohen" || len(got.Files) != 2 || got.Files[1] != moved {
		t.Errorf("combined task = %+v", got)
	}
	if code := combine("old", "busy"); code != 409 || b.st.Tasks["busy"] == nil {
		t.Errorf("a newer task the agent works on: %d, want 409 and kept", code)
	}
	if code := combine("old", "old"); code != 400 {
		t.Errorf("with itself: %d, want 400", code)
	}
	if code := combine("old", "nope"); code != 404 {
		t.Errorf("unknown task: %d, want 404", code)
	}
}

func TestGHRemoteRe(t *testing.T) {
	for remote, want := range map[string]string{"git@github.com:o/r.git": "o/r", "https://github.com/o/r": "o/r",
		"ssh://git@github.com/o/r.git": "o/r", "https://github.com/o/r.git/": "o/r", "git@gitlab.com:o/r.git": ""} {
		got := ""
		if m := ghRemoteRe.FindStringSubmatch(remote); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("ghRemoteRe(%s) = %q, want %q", remote, got, want)
		}
	}
}

// A session's report on its action ends its job, posted as "agent" or as "claude", the name before the rename; Eugene's
// note doesn't.
func TestReportEndsJob(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	for _, actor := range []string{"agent", "claude", "eugene"} {
		b.st.Tasks = map[string]*Task{"t1": {ID: "t1", Job: &Job{Action: "act", Status: "running", Session: "t1-act"}}}
		rec := httptest.NewRecorder()
		b.routes().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/tasks/t1/notes",
			strings.NewReader(`{"actor":"`+actor+`","kind":"act","text":"done"}`)))
		if got, want := b.st.Tasks["t1"].Job.Status, map[bool]string{true: "running", false: "ok"}[actor == "eugene"]; rec.Code != 200 || got != want {
			t.Errorf("%s's report: %d, job %s; want 200, job %s", actor, rec.Code, got, want)
		}
	}
}

// [mcp] servers are a command or a url, and what names one must find it; a feed needs a stdio server, a tool and good keys.
func TestCheckMCP(t *testing.T) {
	b := &Board{home: "/home"}
	servers := map[string]MCPServer{"kith": {Command: "kith-mcp"}, "atlassian": {URL: "https://mcp.example/mcp"}}
	ok := Config{MCP: servers, Areas: []Area{{ID: "work", Dir: "/", MCP: []string{"kith", "atlassian"},
		Runs:  []Run{{Prompt: "p", MCP: []string{}}},
		Feeds: []Feed{{MCP: "kith", Tool: "read_room", Args: map[string]any{"room": "r"}, Keys: `\bBE-\d+\b`}}}}}
	if _, err := b.checkConfig(&ok); err != nil {
		t.Fatalf("checkConfig(good) = %v", err)
	}
	for name, bad := range map[string]func(c *Config){
		"server with both":     func(c *Config) { c.MCP = map[string]MCPServer{"x": {Command: "x", URL: "https://x"}} },
		"server with neither":  func(c *Config) { c.MCP = map[string]MCPServer{"x": {Args: []string{"a"}}} },
		"area names none":      func(c *Config) { c.Areas[0].MCP = []string{"gone"} },
		"run names none":       func(c *Config) { c.Areas[0].Runs[0].MCP = []string{"gone"} },
		"feed on a url server": func(c *Config) { c.Areas[0].Feeds[0].MCP = "atlassian" },
		"feed on none":         func(c *Config) { c.Areas[0].Feeds[0].MCP = "gone" },
		"feed without a tool":  func(c *Config) { c.Areas[0].Feeds[0].Tool = "" },
		"feed with bad keys":   func(c *Config) { c.Areas[0].Feeds[0].Keys = "(" },
	} {
		c := ok
		c.Areas = []Area{ok.Areas[0]}
		c.Areas[0].Runs, c.Areas[0].Feeds = slices.Clone(ok.Areas[0].Runs), slices.Clone(ok.Areas[0].Feeds)
		bad(&c)
		if _, err := b.checkConfig(&c); err == nil {
			t.Errorf("%s: taken", name)
		}
	}

	// An area gets exactly its servers; none listed is the folder's own config, [] is no server at all.
	b.conf, b.areas = ok, ok.Areas
	if f := b.mcpFor("work"); len(f) != 3 || f[0] != "--strict-mcp-config" || f[1] != "--mcp-config" ||
		f[2] != `{"mcpServers":{"atlassian":{"type":"http","url":"https://mcp.example/mcp"},"kith":{"type":"stdio","command":"kith-mcp"}}}` {
		t.Errorf("mcpFor(work) = %q", f)
	}
	if f := b.mcpFlags([]string{}); len(f) != 3 || f[2] != `{"mcpServers":{}}` {
		t.Errorf("mcpFlags([]) = %q, want strict with no servers", f)
	}
	if f := b.mcpFor("none"); f != nil {
		t.Errorf("mcpFor(none) = %q, want no flags", f)
	}
}

// A feed is any stdio MCP tool answering messages: its first read is a baseline, then each new message's keys reach the
// tasks that track them.
func TestPollFeed(t *testing.T) {
	dir := t.TempDir()
	answer := func(bodies ...string) {
		var msgs []map[string]string
		for i, body := range bodies {
			msgs = append(msgs, map[string]string{"event_id": fmt.Sprint("e", i), "body": body})
		}
		text, err := json.Marshal(map[string]any{"messages": msgs})
		if err != nil {
			t.Fatal(err)
		}
		line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "answer"), append(line, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The server's args come from [mcp]; the tool call, as it arrives, is kept in call.
	server := "#!/bin/sh\n[ \"$1\" = --flag ] || exit 1\nwhile read -r line; do case \"$line\" in\n" +
		`*'"id":1'*) echo '{"jsonrpc":"2.0","id":1,"result":{}}';;` + "\n" +
		`*'"id":2'*) printf '%s\n' "$line" > ` + dir + `/call; cat ` + dir + `/answer;;` + "\nesac; done\n"
	if err := os.WriteFile(filepath.Join(dir, "server"), []byte(server), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir}
	b.st.JiraSeen = map[string][]string{}
	b.st.Tasks = map[string]*Task{"a": {ID: "a", Jira: []string{"BE-1"}, Job: &Job{Status: "running"}}} // busy: noted, no run
	area, feed := &Area{ID: "work"}, Feed{MCP: "x", Tool: "list", Args: map[string]any{"room": "r", "limit": 50}}
	srv, keyRe := MCPServer{Command: filepath.Join(dir, "server"), Args: []string{"--flag"}}, regexp.MustCompile(`\bBE-\d+\b`)

	answer("BE-1 moved to In Progress")
	if err := b.pollFeed(srv, area, feed, keyRe); err != nil {
		t.Fatal(err)
	}
	if call, _ := os.ReadFile(filepath.Join(dir, "call")); !strings.Contains(string(call), `"params":{"arguments":{"limit":50,"room":"r"},"name":"list"}`) {
		t.Errorf("tool call %s, want list with the feed's args", call)
	}
	if h := b.st.Tasks["a"].History; len(h) != 0 {
		t.Fatalf("baseline noted %+v, want nothing", h)
	}
	answer("BE-1 moved to In Progress", "BE-1 commented by Ann")
	if err := b.pollFeed(srv, area, feed, keyRe); err != nil {
		t.Fatal(err)
	}
	if h := b.st.Tasks["a"].History; len(h) != 1 || h[0].Kind != "update" || h[0].Text != "BE-1 commented by Ann" {
		t.Errorf("history %+v, want the new message as one update", h)
	}
	if seen := b.st.JiraSeen[feed.id()]; !slices.Equal(seen, []string{"e0", "e1"}) {
		t.Errorf("seen %v, want e0 e1 under the feed's id", seen)
	}
}

// A job runs on its run's agent, else its area's [use], else the board's, else the built-in one; config.toml's [agents]
// add to and override the built-in ones, and a name in no [agents], or another kind, is refused.
func TestAgentFor(t *testing.T) {
	fast := Agent{Kind: "claude-code", Model: "sonnet"}
	b := &Board{conf: Config{Agents: map[string]Agent{"fast": fast}, Use: Use{Decide: "opus"}},
		areas: []Area{{ID: "w", Use: Use{Session: "fast"}}}}
	for _, c := range []struct {
		job, area, run string
		want           Agent
	}{
		{"session", "w", "", fast},
		{"session", "x", "", defaultAgents["opus"]},
		{"decide", "w", "", defaultAgents["opus"]},
		{"headless", "w", "haiku", defaultAgents["haiku"]},
	} {
		if got := b.agentFor(c.job, c.area, c.run); got != c.want {
			t.Errorf("agentFor(%q, %q, %q) = %+v, want %+v", c.job, c.area, c.run, got, c.want)
		}
	}
	if got := (&Board{}).agentFor("decide", "", ""); got != defaultAgents["haiku"] {
		t.Errorf("default decide agent = %+v, want haiku", got)
	}
	for _, c := range []Config{
		{Use: Use{Session: "nope"}},
		{Areas: []Area{{ID: "w", Dir: "/", Use: Use{Decide: "nope"}}}},
		{Areas: []Area{{ID: "w", Dir: "/", Runs: []Run{{Prompt: "p", Agent: "nope"}}}}},
		{Agents: map[string]Agent{"x": {Kind: "cli"}}},
	} {
		if _, err := b.checkConfig(&c); err == nil {
			t.Errorf("checkConfig(%+v) took it", c)
		}
	}
	if _, err := b.checkConfig(&Config{Agents: map[string]Agent{"fast": fast}, Use: Use{Session: "fast", Decide: "opus"}}); err != nil {
		t.Errorf("checkConfig refused known agents: %v", err)
	}
}

func TestMCP(t *testing.T) {
	dir := t.TempDir()
	b := &Board{path: filepath.Join(dir, "state.json"), logDir: dir, areas: []Area{{ID: "work", Name: "Work", Dir: dir}}}
	b.st.Tasks = map[string]*Task{"t1": {ID: "t1", Area: "work", Status: "doing", Urgency: "medium", Importance: "medium"}}
	mux := b.routes()
	call := func(body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(body)))
		var reply map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &reply)
		return rec.Code, reply
	}
	tool := func(name, args string) map[string]any {
		_, reply := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`)
		res, _ := reply["result"].(map[string]any)
		return res
	}
	_, init := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if res, _ := init["result"].(map[string]any); res["protocolVersion"] != "2025-06-18" || res["instructions"] != mcpInstructions {
		t.Errorf("initialize: %v", init)
	}
	if code, _ := call(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Errorf("notification: %d, want 202", code)
	}
	if res := tool("note_task", `{"task":"t1","text":"fixed the flake"}`); res["isError"] != false {
		t.Errorf("note_task: %v", res)
	}
	if res := tool("update_task", `{"task":"t1","fields":{"status":"waiting","waitingFor":"PR review"}}`); res["isError"] != false {
		t.Errorf("update_task: %v", res)
	}
	tk := b.st.Tasks["t1"]
	if tk.Status != "waiting" || tk.WaitingFor != "PR review" || len(tk.History) != 2 ||
		tk.History[0] != (Entry{At: tk.History[0].At, Actor: "agent", Kind: "message", Text: "fixed the flake"}) {
		t.Errorf("task %+v", tk)
	}
	// A wrong id must not create a task, and a bad field is the tool's error, not the protocol's.
	if res := tool("update_task", `{"task":"nope","fields":{"status":"done"}}`); res["isError"] != true || b.st.Tasks["nope"] != nil {
		t.Errorf("unknown task: %v", res)
	}
	if res := tool("update_task", `{"task":"t1","fields":{"urgency":"asap"}}`); res["isError"] != true {
		t.Errorf("bad field: %v", res)
	}
}
