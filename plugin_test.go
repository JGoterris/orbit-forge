package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JGoterris/orbit/pluginsdk"
	"github.com/JGoterris/orbit/pluginsdk/protocol"
)

// fakeForge is a forge with canned answers that records what it was asked.
type fakeForge struct {
	mu    sync.Mutex
	kind  string
	mrs   []MR
	runs  []Run
	jobs  []Job
	log   string
	err   error
	calls []string
}

func (f *fakeForge) rec(format string, a ...any) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
	f.mu.Unlock()
}
func (f *fakeForge) reset() { f.mu.Lock(); f.calls = nil; f.mu.Unlock() }
func (f *fakeForge) did() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
func (f *fakeForge) Kind() string { return f.kind }
func (f *fakeForge) Prefix() string {
	if f.kind == "gitlab" {
		return "!"
	}
	return "#"
}
func (f *fakeForge) ListMRs(*Repo) ([]MR, error) { f.rec("ListMRs"); return f.mrs, f.err }
func (f *fakeForge) ViewMR(_ *Repo, id string) (MR, error) {
	f.rec("ViewMR %s", id)
	for _, m := range f.mrs {
		if m.ID == id {
			return m, f.err
		}
	}
	return MR{}, errors.New("no such merge request")
}
func (f *fakeForge) ListRuns(r *Repo, branch string) ([]Run, error) {
	f.rec("ListRuns %q", branch)
	var out []Run
	for _, x := range f.runs {
		if branch == "" || x.Branch == branch {
			out = append(out, x)
		}
	}
	return out, f.err
}
func (f *fakeForge) Jobs(_ *Repo, id string) ([]Job, error) {
	f.rec("Jobs %s", id)
	return f.jobs, f.err
}
func (f *fakeForge) Log(_ *Repo, id string, failed bool) (string, error) {
	f.rec("Log %s %v", id, failed)
	return f.log, f.err
}
func (f *fakeForge) Approve(_ *Repo, id string) error  { f.rec("Approve %s", id); return f.err }
func (f *fakeForge) Merge(_ *Repo, id, m string) error { f.rec("Merge %s %s", id, m); return f.err }
func (f *fakeForge) Close(_ *Repo, id string) error    { f.rec("Close %s", id); return f.err }
func (f *fakeForge) Checkout(_ *Repo, id string) error { f.rec("Checkout %s", id); return f.err }
func (f *fakeForge) Rerun(_ *Repo, id string, failed bool) error {
	f.rec("Rerun %s %v", id, failed)
	return f.err
}
func (f *fakeForge) Cancel(_ *Repo, id string) error { f.rec("Cancel %s", id); return f.err }
func (f *fakeForge) Comment(_ *Repo, id, body string) error {
	f.rec("Comment %s %s", id, body)
	return f.err
}
func (f *fakeForge) SetDraft(_ *Repo, id string, draft bool) error {
	f.rec("SetDraft %s %v", id, draft)
	return f.err
}
func (f *fakeForge) Workflows(*Repo) ([]Workflow, error) {
	return []Workflow{{ID: "ci.yml", Name: "CI (ci.yml)"}, {ID: "deploy.yml", Name: "Deploy (deploy.yml)"}}, f.err
}
func (f *fakeForge) Start(_ *Repo, wf, branch string) error {
	f.rec("Start %s %s", wf, branch)
	return f.err
}
func (f *fakeForge) Branches(*Repo) ([]string, error) {
	return []string{"main", "feature", "other"}, f.err
}

// recorder is what the plugin told orbit on its own.
type recorder struct {
	mu      sync.Mutex
	badges  map[string][]protocol.Badge
	sets    int
	header  []protocol.Badge
	toasts  []string
	toneOf  map[string]protocol.Tone
	invalid int
}

func newRecorder() *recorder {
	return &recorder{badges: map[string][]protocol.Badge{}, toneOf: map[string]protocol.Tone{}}
}
func (r *recorder) SetServiceBadges(s string, b ...protocol.Badge) {
	r.mu.Lock()
	r.badges[s] = b
	r.sets++
	r.mu.Unlock()
}
func (r *recorder) SetHeaderBadges(b ...protocol.Badge) { r.mu.Lock(); r.header = b; r.mu.Unlock() }
func (r *recorder) Invalidate(string, string)           { r.mu.Lock(); r.invalid++; r.mu.Unlock() }
func (r *recorder) Toast(t protocol.Tone, s string) {
	r.mu.Lock()
	r.toasts = append(r.toasts, s)
	r.toneOf[s] = t
	r.mu.Unlock()
}

const sample = "/work/orbit"

// newTestApp is an app over a repository with branch "feature" on a fake forge; the service "web" is in no repository.
func newTestApp(t *testing.T, f *fakeForge) (*app, *recorder) {
	t.Helper()
	a := newApp()
	rec := newRecorder()
	a.n = rec
	a.p = pluginsdk.New()
	a.afterAction = func() {} // the tests look at what an action did, not at the refresh that follows
	a.register()
	a.repos.git = func(dir string, args ...string) (string, error) {
		if dir == "/norepo" {
			return "", errors.New("not a repo")
		}
		switch strings.Join(args, " ") {
		case "rev-parse --show-toplevel":
			return sample, nil
		case "branch --show-current":
			return "feature", nil
		case "config branch.feature.remote":
			return "orbit", nil
		case "remote get-url orbit":
			return "git@github.com:owner/name.git", nil
		}
		return "", errors.New("unexpected git " + strings.Join(args, " "))
	}
	a.repos.pick = func(string) Forge { return f }
	a.setServices([]protocol.ServiceRef{
		{Name: "api", Type: "process", Cwd: sample + "/api"},
		{Name: "worker", Type: "process", Cwd: sample + "/worker"},
		{Name: "web", Type: "process", Cwd: "/norepo"},
	})
	return a, rec
}

func mrs() []MR {
	return []MR{
		{ID: "41", Title: "Fix login", Author: "ana", Branch: "feature", Target: "main", State: "open", Review: "approved", Checks: "pass",
			URL: "https://x/41", Body: "line1\nline2", Updated: time.Now().Add(-3 * time.Hour),
			CheckList: []Check{{"build", "pass"}, {"lint", "fail"}}},
		{ID: "42", Title: "Draft", Author: "bo", Branch: "other", Target: "main", State: "draft", Checks: "fail"},
	}
}

func req(service string, selection map[string]string) pluginsdk.Request {
	return pluginsdk.Request{Context: protocol.Context{Service: service, Selection: selection}}
}

func TestMRListShowsTheMergeRequestsOfTheRepoOfTheService(t *testing.T) {
	a, _ := newTestApp(t, &fakeForge{kind: "github", mrs: mrs()})
	d, err := a.mrList(req("api", nil))
	if err != nil || d.List == nil || len(d.List.Items) != 2 {
		t.Fatalf("%+v %v", d, err)
	}
	first := d.List.Items[0]
	if first.ID != "41" || first.Icon != "open" || first.Cells["title"].Text != "#41 Fix login" ||
		first.Cells["checks"].Text != "pass" || first.Cells["checks"].Tone != protocol.ToneOK || first.Cells["review"].Tone != protocol.ToneOK {
		t.Fatalf("first = %+v", first)
	}
	if d.List.Items[1].Icon != "draft" || d.List.Items[1].Cells["checks"].Tone != protocol.ToneError {
		t.Fatalf("second = %+v", d.List.Items[1])
	}
	if d.List.Footer != "owner/name · github" || len(d.List.Columns) == 0 || d.List.Columns[0].ID != "title" {
		t.Fatalf("list = %+v", d.List)
	}
}

func TestGitLabNumbersWithABang(t *testing.T) {
	a, _ := newTestApp(t, &fakeForge{kind: "gitlab", mrs: mrs()})
	d, _ := a.mrList(req("api", nil))
	if d.List.Items[0].Cells["title"].Text != "!41 Fix login" {
		t.Fatalf("title = %q", d.List.Items[0].Cells["title"].Text)
	}
}

func TestAServiceOutsideAnyRepoSaysSo(t *testing.T) {
	a, _ := newTestApp(t, &fakeForge{kind: "github"})
	d, err := a.mrList(req("web", nil))
	if err != nil || len(d.List.Items) != 0 || d.List.Empty != "web is not in a git repository" {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := a.mrList(req("", nil)); err == nil {
		t.Fatal("no service selected is a mistake the pane shows")
	}
}

func TestForgeErrorsReachThePane(t *testing.T) {
	a, _ := newTestApp(t, &fakeForge{kind: "github", err: errors.New("gh: HTTP 401")})
	if _, err := a.mrList(req("api", nil)); err == nil || err.Error() != "gh: HTTP 401" {
		t.Fatalf("%v", err)
	}
}

func TestMRInfoDetailsTheSelectedOne(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs()}
	a, _ := newTestApp(t, f)
	if d, _ := a.mrInfo(req("api", nil)); d.Detail == nil || len(d.Detail.Sections) != 0 {
		t.Fatal("nothing selected: nothing to show, and no call to the forge")
	}
	if len(f.did()) != 0 {
		t.Fatalf("calls = %v", f.did())
	}
	d, err := a.mrInfo(req("api", map[string]string{"list": "41"}))
	if err != nil || len(d.Detail.Sections) != 3 {
		t.Fatalf("%+v %v", d, err)
	}
	head, checks, desc := d.Detail.Sections[0], d.Detail.Sections[1], d.Detail.Sections[2]
	if head.Title != "#41 Fix login" || checks.Title != "Checks · 1 pass · 1 fail" || desc.Title != "Description" || len(desc.Text) != 2 {
		t.Fatalf("head = %+v checks = %+v desc = %+v", head, checks, desc)
	}
	got := map[string]string{}
	for _, f := range head.Fields {
		got[f.Label] = f.Value
	}
	if got["state"] != "open" || got["branch"] != "feature → main" || got["review"] != "approved" || got["checks"] != "pass" || got["updated"] != "3h ago" {
		t.Fatalf("fields = %v", got)
	}
	// the failing ones first
	if first := checks.Text[0]; first[1].Text != "lint" || first[0].Tone != protocol.ToneError {
		t.Fatalf("checks = %+v", checks.Text)
	}
}

func TestCIListIsTheBranchOfTheServiceUnlessAllIsOn(t *testing.T) {
	f := &fakeForge{kind: "github", runs: []Run{
		{ID: "1", Name: "CI", Title: "a", Branch: "feature", Status: "failure", Event: "push", URL: "https://x/1"},
		{ID: "2", Name: "CI", Title: "b", Branch: "main", Status: "success"},
	}}
	a, _ := newTestApp(t, f)
	d, _ := a.ciList(req("api", nil))
	if len(d.List.Items) != 1 || d.List.Items[0].Icon != "fail" || !strings.Contains(d.List.Footer, "branch feature") {
		t.Fatalf("%+v", d.List)
	}
	res, err := a.p0(t, "ci-all")
	if err != nil || res.Invalidate[0] != "ci" {
		t.Fatalf("%+v %v", res, err)
	}
	d, _ = a.ciList(req("api", nil))
	if len(d.List.Items) != 2 || !strings.Contains(d.List.Footer, "all the branches") {
		t.Fatalf("%+v", d.List)
	}
}

// p0 runs an action of the plugin by id, the way the SDK would.
func (a *app) p0(t *testing.T, id string) (*protocol.InvokeResult, error) {
	t.Helper()
	return a.act(id, pluginsdk.ActionRequest{Context: protocol.Context{Service: "api"}})
}

func TestCILogShowsTheJobsAndTheFailedOnes(t *testing.T) {
	f := &fakeForge{kind: "github", jobs: []Job{{ID: "1", Name: "build", Status: "success"}, {ID: "2", Name: "test", Status: "failure", Steps: []Step{{"checkout", "success"}, {"go test", "failure"}}}}, log: "step ok\n\x1b[31mERROR: boom\x1b[0m\nlast"}
	a, _ := newTestApp(t, f)
	d, err := a.ciLog(req("api", map[string]string{"list": "7"}))
	if err != nil || d.Text == nil {
		t.Fatalf("%+v %v", d, err)
	}
	var text []string
	for _, l := range d.Text.Lines {
		var s string
		for _, sp := range l {
			s += sp.Text
		}
		text = append(text, s)
	}
	all := strings.Join(text, "\n")
	for _, want := range []string{"✖ test", "✖ go test", "✓ 1 step passed", "output of the failed jobs", "ERROR: boom", "last"} {
		if !strings.Contains(all, want) {
			t.Errorf("the log lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "\x1b") {
		t.Error("colour codes must be dropped")
	}
	if strings.Contains(all, "build") {
		t.Error("a job that passed is in the graph, not among the failures")
	}
	f.jobs = []Job{{ID: "1", Name: "build", Status: "success"}}
	a.jobs = map[string]cachedJobs{}
	f.reset()
	a.ciLog(req("api", map[string]string{"list": "7"}))
	for _, c := range f.did() {
		if strings.HasPrefix(c, "Log") {
			t.Fatal("a run with nothing failed must not fetch the failed log")
		}
	}
}

func TestMergeAsksTheForgeWithTheChosenMethod(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs()}
	a, _ := newTestApp(t, f)
	res, err := a.act("mr-merge", pluginsdk.ActionRequest{Context: protocol.Context{Service: "api", Selection: map[string]string{"list": "41"}}, Inputs: map[string]string{"method": "squash"}})
	if err != nil || res.Toast.Text != "merged #41" || res.Invalidate[0] != "mr" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := f.did(); got[0] != "Merge 41 squash" {
		t.Fatalf("calls = %v", got)
	}
	for id, want := range map[string]string{"mr-approve": "Approve 41", "mr-close": "Close 41", "mr-checkout": "Checkout 41"} {
		f.reset()
		if _, err := a.act(id, pluginsdk.ActionRequest{Context: protocol.Context{Service: "api", Selection: map[string]string{"list": "41"}}}); err != nil || f.did()[0] != want {
			t.Errorf("%s: %v %v", id, f.did(), err)
		}
	}
}

func TestActionsNeedASelectionAndReportTheFailure(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs()}
	a, _ := newTestApp(t, f)
	if _, err := a.act("mr-merge", pluginsdk.ActionRequest{Context: protocol.Context{Service: "api"}}); err == nil || !strings.Contains(err.Error(), "select") {
		t.Fatalf("%v", err)
	}
	if len(f.did()) != 0 {
		t.Fatal("nothing must be asked of the forge without a selection")
	}
	f.err = errors.New("gh: not allowed")
	_, err := a.act("mr-close", pluginsdk.ActionRequest{Context: protocol.Context{Service: "api", Selection: map[string]string{"list": "41"}}})
	if err == nil || err.Error() != "gh: not allowed" {
		t.Fatalf("the error of the tool is the one the user reads: %v", err)
	}
}

func TestRerunOnlyTheFailedJobsOfAFailedRun(t *testing.T) {
	f := &fakeForge{kind: "github", runs: []Run{
		{ID: "1", Branch: "feature", Status: "failure"}, {ID: "2", Branch: "feature", Status: "success"},
	}}
	a, _ := newTestApp(t, f)
	a.ciList(req("api", nil)) // learns the runs
	for id, want := range map[string]string{"1": "Rerun 1 true", "2": "Rerun 2 false"} {
		f.reset()
		res, err := a.act("ci-rerun", pluginsdk.ActionRequest{Context: protocol.Context{Service: "api", Selection: map[string]string{"list": id}}})
		if err != nil || res.Invalidate[0] != "ci" {
			t.Fatalf("%v %v", res, err)
		}
		if got := f.did()[0]; got != want {
			t.Errorf("run %s: %q, want %q", id, got, want)
		}
	}
	f.reset()
	if _, err := a.act("ci-cancel", pluginsdk.ActionRequest{Context: protocol.Context{Service: "api", Selection: map[string]string{"list": "1"}}}); err != nil || f.did()[0] != "Cancel 1" {
		t.Fatalf("%v %v", f.did(), err)
	}
}

func TestOpenInTheBrowserUsesWhatTheListLearned(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs(), runs: []Run{{ID: "9", Branch: "feature", URL: "https://x/runs/9"}}}
	a, _ := newTestApp(t, f)
	var opened []string
	a.open = func(u string) error { opened = append(opened, u); return nil }
	sel := map[string]string{"list": "41"}
	if _, err := a.act("mr-open", pluginsdk.ActionRequest{Context: protocol.Context{Service: "api", Selection: sel}}); err == nil {
		t.Fatal("before the list was read the page is not known: say so, do not open nothing")
	}
	a.mrList(req("api", nil))
	res, err := a.act("mr-open", pluginsdk.ActionRequest{Context: protocol.Context{Service: "api", Selection: sel}})
	if err != nil || len(opened) != 1 || opened[0] != "https://x/41" || res.Invalidate[0] != "none" {
		t.Fatalf("%v %v %+v", opened, err, res)
	}
	a.ciList(req("api", nil))
	if _, err := a.act("ci-open", pluginsdk.ActionRequest{Context: protocol.Context{Service: "api", Selection: map[string]string{"list": "9"}}}); err != nil || opened[1] != "https://x/runs/9" {
		t.Fatalf("%v %v", opened, err)
	}
}

func TestBadgesOneQueryPerRepoNotPerService(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs(), runs: []Run{{ID: "1", Branch: "feature", Status: "failure"}}}
	a, rec := newTestApp(t, f)
	a.refresh()
	queries := 0
	for _, c := range f.did() {
		if c == "ListMRs" {
			queries++
		}
	}
	if queries != 1 {
		t.Fatalf("api and worker share a repository: %d queries (%v)", queries, f.did())
	}
	for _, svc := range []string{"api", "worker"} {
		b := rec.badges[svc]
		if len(b) != 1 || b[0].Text != "#41" || b[0].Icon != "ok" || b[0].Tone != protocol.ToneOK {
			t.Errorf("%s badge = %+v", svc, b)
		}
	}
	if b := rec.badges["web"]; len(b) != 0 {
		t.Errorf("a service outside a repo gets no badge: %+v", b)
	}
	if len(rec.header) != 1 || rec.header[0].Text != "CI 2" || rec.header[0].Tone != protocol.ToneError {
		t.Errorf("two services on a failing branch: %+v", rec.header)
	}
	if rec.invalid == 0 {
		t.Error("what is shown must be asked again when the badges change")
	}
}

func TestBadgesAreOnlySentWhenTheyChange(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs(), runs: []Run{{ID: "1", Branch: "feature", Status: "success"}}}
	a, rec := newTestApp(t, f)
	a.refresh()
	sets, invalid := rec.sets, rec.invalid
	a.refresh()
	if rec.sets != sets || rec.invalid != invalid {
		t.Fatalf("nothing changed, nothing must be sent (%d → %d, %d → %d)", sets, rec.sets, invalid, rec.invalid)
	}
	f.mu.Lock()
	f.mrs[0].Checks = "fail"
	f.mu.Unlock()
	a.refresh()
	if rec.badges["api"][0].Icon != "fail" || rec.sets == sets {
		t.Fatalf("a change must be sent: %+v", rec.badges["api"])
	}
}

func TestACIRunThatFinishesIsToldOnce(t *testing.T) {
	f := &fakeForge{kind: "github", runs: []Run{{ID: "1", Branch: "feature", Status: "running"}}}
	a, rec := newTestApp(t, f)
	a.refresh()
	if len(rec.toasts) != 0 || rec.header[0].Icon != "run" {
		t.Fatalf("a run in progress is not news: %v %+v", rec.toasts, rec.header)
	}
	f.mu.Lock()
	f.runs[0].Status = "failure"
	f.mu.Unlock()
	a.refresh()
	want := "CI failed on api (feature)"
	if len(rec.toasts) == 0 || rec.toasts[0] != want || rec.toneOf[want] != protocol.ToneError {
		t.Fatalf("toasts = %v", rec.toasts)
	}
	n := len(rec.toasts)
	a.refresh()
	if len(rec.toasts) != n {
		t.Fatal("the same run must not be announced again")
	}
}

func TestBadgeOfABranchWithoutMergeRequestIsItsCI(t *testing.T) {
	f := &fakeForge{kind: "github", runs: []Run{{ID: "1", Branch: "feature", Status: "success"}}}
	a, rec := newTestApp(t, f)
	a.refresh()
	b := rec.badges["api"]
	if len(b) != 1 || b[0].Text != "CI" || b[0].Icon != "ok" {
		t.Fatalf("%+v", b)
	}
	if len(rec.header) != 1 || rec.header[0].Text != "CI" || rec.header[0].Tone != protocol.ToneOK {
		t.Fatalf("%+v", rec.header)
	}
}

func TestSectionOfTheDetailComesFromTheLastPoll(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs()}
	a, _ := newTestApp(t, f)
	if secs, _ := a.section("api"); len(secs) != 0 {
		t.Fatal("before the first poll there is nothing to say")
	}
	a.refresh()
	f.reset()
	secs, err := a.section("api")
	if err != nil || len(secs) != 1 || secs[0].Fields[0].Label != "#41" || secs[0].Fields[0].Value != "Fix login" {
		t.Fatalf("%+v %v", secs, err)
	}
	if len(f.did()) != 0 {
		t.Fatal("selecting a service must not hit the forge: it is the poll that does")
	}
}

func TestFailuresPutTheErrorsFirstAndFoldTheSetUp(t *testing.T) {
	out := "── test\n##[group]Run actions/setup-go@v5\nwith:\n  go-version: 1.26\n##[endgroup]\n/usr/bin/tar: Cannot open: File exists\n" +
		"##[group]Run go test ./...\ngo test ./...\n##[endgroup]\n" +
		"--- FAIL: TestLogin (0.01s)\n    login_test.go:42: got 401\n##[warning]Node.js 20 is deprecated\n##[error]Process completed with exit code 1."
	got := render(failureLines(out))
	want := `
── errors
✖ --- FAIL: TestLogin (0.01s)
✖ login_test.go:42: got 401
✖ Process completed with exit code 1.

── output of the step that failed
▸ Run go test ./...
--- FAIL: TestLogin (0.01s)
    login_test.go:42: got 401
▲ Node.js 20 is deprecated
##[error]Process completed with exit code 1.
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRunTitleOfAMerge(t *testing.T) {
	if got := runTitle("Merge pull request #10 from JGoterris/feature/plugins"); got != "#10 feature/plugins" {
		t.Fatalf("got %q", got)
	}
	if got := runTitle("fix: something"); got != "fix: something" {
		t.Fatalf("got %q", got)
	}
}

func TestFailuresFoldTheSameMessageOverAndOver(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "/usr/bin/tar: ../../../go/pkg/mod/golang.org/toolchain/src/image/testdata/file-%d.png: Cannot open: File exists\n", i)
	}
	b.WriteString("--- FAIL: TestX")
	got := render(failureLines(b.String()))
	if !strings.Contains(got, "file-0.png") || strings.Contains(got, "file-1.png") || !strings.Contains(got, "… and 29 more like it") {
		t.Fatalf("got\n%s", got)
	}
}
