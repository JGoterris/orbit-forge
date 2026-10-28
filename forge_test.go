package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseRemote(t *testing.T) {
	for in, want := range map[string][2]string{
		"https://github.com/JGoterris/orbit":              {"github.com", "JGoterris/orbit"},
		"https://github.com/JGoterris/orbit.git":          {"github.com", "JGoterris/orbit"},
		"git@github.com:JGoterris/orbit.git":              {"github.com", "JGoterris/orbit"},
		"ssh://git@gitlab.example.com:2222/group/sub/app": {"gitlab.example.com", "group/sub/app"},
		"git@gitlab.example.com:group/sub/app.git":        {"gitlab.example.com", "group/sub/app"},
		"https://user:tok@gitlab.com/g/p.git/":            {"gitlab.com", "g/p"},
	} {
		host, slug, err := parseRemote(in)
		if err != nil || host != want[0] || slug != want[1] {
			t.Errorf("%q -> %q %q %v, want %v", in, host, slug, err, want)
		}
	}
	if _, _, err := parseRemote("not a remote"); err == nil {
		t.Error("garbage must be refused")
	}
}

func TestForgeForHost(t *testing.T) {
	t.Setenv("ORBIT_FORGE", "")
	if forgeFor("github.com").Kind() != "github" || forgeFor("gitlab.com").Kind() != "gitlab" || forgeFor("git.company.io").Kind() != "gitlab" {
		t.Fatal("github.com is GitHub and any other host is taken for a GitLab")
	}
	t.Setenv("ORBIT_FORGE", "gh")
	if forgeFor("git.company.io").Kind() != "github" {
		t.Fatal("ORBIT_FORGE decides")
	}
}

func TestRepoOfUsesTheRemoteTheBranchTracksAndNotOnlyOrigin(t *testing.T) {
	answers := map[string]string{
		"rev-parse --show-toplevel":    "/work/orbit",
		"branch --show-current":        "feature",
		"config branch.feature.remote": "orbit",
		"remote get-url orbit":         "https://github.com/JGoterris/orbit",
		"remote":                       "orbit\nbackup",
		"remote get-url origin":        "https://github.com/someone/else",
	}
	r := newRepos()
	r.git = func(dir string, args ...string) (string, error) {
		if v, ok := answers[strings.Join(args, " ")]; ok {
			return v, nil
		}
		return "", os.ErrNotExist
	}
	repo, err := r.of("/work/orbit/api")
	if err != nil || repo.Slug != "JGoterris/orbit" || repo.Branch != "feature" || repo.Forge.Kind() != "github" {
		t.Fatalf("repo = %+v, %v", repo, err)
	}
	// no tracking config: origin, then the first remote
	delete(answers, "config branch.feature.remote")
	delete(answers, "remote get-url orbit")
	answers["remote"] = "backup\norigin"
	r = newRepos()
	r.git = func(dir string, args ...string) (string, error) {
		if v, ok := answers[strings.Join(args, " ")]; ok {
			return v, nil
		}
		return "", os.ErrNotExist
	}
	if repo, err := r.of("/x"); err != nil || repo.Slug != "someone/else" {
		t.Fatalf("origin must win over the first remote: %+v %v", repo, err)
	}
}

func TestRepoOfReportsWhyThereIsNoForge(t *testing.T) {
	r := newRepos()
	r.git = func(string, ...string) (string, error) { return "", os.ErrNotExist }
	if _, err := r.of("/tmp"); err != errNoRepo {
		t.Fatalf("not a repository: %v", err)
	}
	if _, err := r.of(""); err != errNoRepo {
		t.Fatalf("no directory: %v", err)
	}
	r = newRepos()
	r.git = func(_ string, args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return "/r", nil
		}
		return "", nil
	}
	if _, err := r.of("/r"); err == nil || !strings.Contains(err.Error(), "no remote") {
		t.Fatalf("a repository without a remote must say so: %v", err)
	}
}

// fakeTool puts a script called name first in PATH. It logs its arguments and prints the file the script picks.
func fakeTool(t *testing.T, name, script string) (calls func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake tools are shell scripts")
	}
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	data, _ := filepath.Abs("testdata")
	body := "#!/bin/sh\necho \"$*\" >> \"" + log + "\"\nD=\"" + data + "\"\n" + script
	if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func last(calls []string) string { return calls[len(calls)-1] }

func TestGitHubListsMergeRequests(t *testing.T) {
	calls := fakeTool(t, "gh", `case "$*" in *"pr list"*) cat "$D/gh-pr-list.json";; esac`)
	repo := &Repo{Root: t.TempDir(), Slug: "owner/name"}
	mrs, err := github{}.ListMRs(repo)
	if err != nil || len(mrs) != 3 {
		t.Fatalf("%v %v", mrs, err)
	}
	a, b, c := mrs[0], mrs[1], mrs[2]
	if a.ID != "41" || a.State != "open" || a.Checks != "pass" || a.Review != "approved" || a.Author != "ana" || a.Branch != "fix/login" || a.Target != "main" {
		t.Errorf("first = %+v", a)
	}
	if b.State != "draft" || b.Checks != "fail" || b.Review != "" {
		t.Errorf("a draft with a failing check = %+v", b)
	}
	if c.Checks != "run" || c.Review != "review" || c.Mergeable != "conflicts" {
		t.Errorf("a running check = %+v", c)
	}
	if got := last(calls()); !strings.Contains(got, "pr list --state open") || !strings.HasSuffix(got, "-R owner/name") {
		t.Errorf("gh must be told which repository: %q", got)
	}
}

func TestGitHubViewsOneMergeRequestWithAllItHas(t *testing.T) {
	calls := fakeTool(t, "gh", `case "$*" in *"pr view"*) cat "$D/gh-pr-view.json";; esac`)
	mr, err := github{}.ViewMR(&Repo{Root: t.TempDir(), Slug: "o/n"}, "41")
	if err != nil || !strings.Contains(mr.Body, "Fixes the **login**") || len(mr.CheckList) != 2 || mr.CheckList[1].Status != "skip" {
		t.Fatalf("%+v %v", mr, err)
	}
	if mr.Additions != 120 || mr.Deletions != 30 || mr.Files != 2 || len(mr.FileList) != 2 || mr.FileList[0].Path != "login.go" ||
		strings.Join(mr.Labels, ",") != "bug,auth" || mr.Milestone != "v2" || mr.AutoMerge || mr.Created.IsZero() {
		t.Errorf("what it changes: %+v", mr)
	}
	if len(mr.CommitList) != 1 || mr.CommitList[0].Author != "ana" || len(mr.Notes) != 1 || mr.Notes[0].Author != "bo" || mr.Comments != 1 {
		t.Errorf("commits and comments: %+v %+v", mr.CommitList, mr.Notes)
	}
	var revs []string
	for _, r := range mr.Reviewers {
		revs = append(revs, r.Name+"="+r.State)
	}
	if strings.Join(revs, " ") != "bo=approved cy=changes core-team=pending" {
		t.Errorf("who reviewed, then who is asked to and has not: %v", revs)
	}
	if got := last(calls()); !strings.Contains(got, "latestReviews") || !strings.Contains(got, "files") {
		t.Errorf("the detail asks for more than the list: %q", got)
	}
}

func TestGitHubRunsJobsAndLogs(t *testing.T) {
	calls := fakeTool(t, "gh", `case "$*" in
*"run list"*) cat "$D/gh-run-list.json";;
*"--json jobs"*) cat "$D/gh-run-jobs.json";;
*"--log-failed"*) printf 'test\tRun\terror: boom\n';;
esac`)
	repo := &Repo{Root: t.TempDir(), Slug: "owner/name"}
	runs, err := github{}.ListRuns(repo, "main")
	if err != nil || len(runs) != 5 {
		t.Fatalf("%v %v", runs, err)
	}
	for i, want := range []string{"success", "failure", "running", "queued", "cancelled"} {
		if runs[i].Status != want {
			t.Errorf("run %d is %q, want %q", i, runs[i].Status, want)
		}
	}
	if got := last(calls()); !strings.Contains(got, "--branch main") {
		t.Errorf("the branch must be asked for: %q", got)
	}
	if _, err := (github{}).ListRuns(repo, ""); err != nil || strings.Contains(last(calls()), "--branch") {
		t.Errorf("no branch: all of them: %q", last(calls()))
	}
	jobs, err := github{}.Jobs(repo, "502")
	if err != nil || len(jobs) != 2 || jobs[1].Status != "failure" || jobs[0].ID != "9001" || jobs[0].Duration() != 62*time.Second ||
		len(jobs[1].Steps) != 1 || jobs[1].Steps[0].Status != "failure" {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	if jobs[1].Needs != nil {
		t.Fatalf("without the workflow nothing is known of the needs: %+v", jobs[1])
	}
	log, err := github{}.Log(repo, "502", true)
	if err != nil || !strings.Contains(log, "error: boom") || !strings.Contains(last(calls()), "run view 502 --log-failed") {
		t.Fatalf("log = %q %v %q", log, err, last(calls()))
	}
}

func TestGitHubReadsTheNeedsFromTheWorkflowOfTheRun(t *testing.T) {
	calls := fakeTool(t, "gh", `case "$*" in
*"--json jobs"*) cat "$D/gh-run-jobs.json";;
*"actions/runs/503"*) echo '{"path":".github/workflows/ci.yml","head_sha":"cafe"}';;
*"contents/.github/workflows/ci.yml?ref=cafe"*) printf 'jobs:\n  build: {}\n  test:\n    needs: build\n';;
esac`)
	repo := &Repo{Root: t.TempDir(), Slug: "owner/name"}
	jobs, err := github{}.Jobs(repo, "503")
	if err != nil || len(jobs) != 2 || strings.Join(jobs[1].Needs, ",") != "build" || jobs[0].Needs == nil {
		t.Fatalf("jobs = %+v %v", jobs, err)
	}
	before := len(calls())
	github{}.Jobs(repo, "503")
	for _, c := range calls()[before:] {
		if strings.Contains(c, "contents/") {
			t.Fatal("a workflow at a commit is read once")
		}
	}
}

func TestGitHubWorkflowsAreTheOnesThatCanBeStarted(t *testing.T) {
	fakeTool(t, "gh", `echo '[{"id":1,"name":"CI","path":".github/workflows/ci.yml","state":"active"},{"id":2,"name":"Old","path":".github/workflows/old.yml","state":"disabled_manually"},{"id":3,"name":"Dependabot Updates","path":"dynamic/dependabot/dependabot-updates","state":"active"}]'`)
	wfs, err := github{}.Workflows(&Repo{Root: t.TempDir(), Slug: "o/n"})
	if err != nil || len(wfs) != 1 || wfs[0].ID != "ci.yml" || wfs[0].Name != "CI (ci.yml)" {
		t.Fatalf("%+v %v", wfs, err)
	}
}

func TestGitHubActionsRunTheRightCommands(t *testing.T) {
	calls := fakeTool(t, "gh", "")
	repo := &Repo{Root: t.TempDir(), Slug: "owner/name"}
	g := github{}
	for want, do := range map[string]func() error{
		"pr review 41 --approve -R owner/name":            func() error { return g.Approve(repo, "41") },
		"pr close 41 -R owner/name":                       func() error { return g.Close(repo, "41") },
		"pr checkout 41 -R owner/name":                    func() error { return g.Checkout(repo, "41") },
		"pr merge 41 --squash -R owner/name":              func() error { return g.Merge(repo, "41", "squash") },
		"pr merge 41 --rebase -R owner/name":              func() error { return g.Merge(repo, "41", "rebase") },
		"pr merge 41 --merge -R owner/name":               func() error { return g.Merge(repo, "41", "merge") },
		"run rerun 7 --failed -R owner/name":              func() error { return g.Rerun(repo, "7", true) },
		"run rerun 7 -R owner/name":                       func() error { return g.Rerun(repo, "7", false) },
		"run cancel 7 -R owner/name":                      func() error { return g.Cancel(repo, "7") },
		"pr comment 41 --body hi there -R owner/name":     func() error { return g.Comment(repo, "41", "hi there") },
		"pr ready 41 -R owner/name":                       func() error { return g.SetDraft(repo, "41", false) },
		"pr ready 41 --undo -R owner/name":                func() error { return g.SetDraft(repo, "41", true) },
		"workflow run ci.yml --ref feature -R owner/name": func() error { return g.Start(repo, "ci.yml", "feature") },
	} {
		if err := do(); err != nil {
			t.Errorf("%s: %v", want, err)
			continue
		}
		if got := last(calls()); got != want {
			t.Errorf("ran %q, want %q", got, want)
		}
	}
	before := len(calls())
	if err := g.Merge(repo, "41", "yolo; rm -rf /"); err == nil || len(calls()) != before {
		t.Fatal("an unknown merge method must not reach gh")
	}
}

func TestGitHubLogIsReadable(t *testing.T) {
	got := ghLog("test\tRun go test\t2026-10-27T22:50:14.8586088Z --- FAIL: TestX\ntest\tRun go test\t2026-10-27T22:50:15Z FAIL\nlint\tUNKNOWN STEP\t2026-10-27T22:50:16Z ok")
	want := "── test › Run go test\n--- FAIL: TestX\nFAIL\n── lint\nok"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

func TestToolErrorsSaySomethingUseful(t *testing.T) {
	fakeTool(t, "gh", `echo "HTTP 401: Bad credentials" >&2; echo more >&2; exit 1`)
	_, err := github{}.ListMRs(&Repo{Root: t.TempDir(), Slug: "o/n"})
	if err == nil || err.Error() != "gh: HTTP 401: Bad credentials" {
		t.Fatalf("the first line of what the tool said: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := (github{}).ListMRs(&Repo{Root: t.TempDir(), Slug: "o/n"}); err == nil || err.Error() != "gh is not installed" {
		t.Fatalf("a missing tool: %v", err)
	}
}

func TestGitLabListsAndMapsStatuses(t *testing.T) {
	calls := fakeTool(t, "glab", `case "$*" in
*merge_requests*) cat "$D/gl-mrs.json";;
*"/pipelines/9/jobs"*|*"/pipelines/9/jobs?"*) cat "$D/gl-jobs.json";;
*"/pipelines?"*) cat "$D/gl-pipelines.json";;
esac`)
	repo := &Repo{Root: t.TempDir(), Slug: "owner/name"}
	mrs, err := gitlab{}.ListMRs(repo)
	if err != nil || len(mrs) != 3 {
		t.Fatalf("%v %v", mrs, err)
	}
	if mrs[0].Checks != "pass" || mrs[1].State != "draft" || mrs[1].Checks != "fail" || mrs[2].Checks != "" || mrs[0].Mergeable != "ready" || mrs[1].Mergeable != "ci must pass" || mrs[2].Mergeable != "" {
		t.Errorf("mrs = %+v", mrs)
	}
	if got := last(calls()); !strings.HasPrefix(got, "api projects/owner%2Fname/merge_requests?state=opened") {
		t.Errorf("the project goes url-encoded in the path: %q", got)
	}
	runs, _ := gitlab{}.ListRuns(repo, "main")
	if len(runs) != 3 || runs[0].Status != "failure" || runs[1].Status != "running" || runs[2].Status != "cancelled" || runs[0].Name != "pipeline #9" {
		t.Errorf("runs = %+v", runs)
	}
	if got := last(calls()); !strings.Contains(got, "ref=main") {
		t.Errorf("the branch: %q", got)
	}
	jobs, _ := gitlab{}.Jobs(repo, "9")
	if len(jobs) != 3 || jobs[0].Status != "success" || jobs[1].Status != "failure" || jobs[2].Status != "queued" {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestGitLabViewsOneMergeRequestWithAllItHas(t *testing.T) {
	fakeTool(t, "glab", `case "$*" in
*"merge_requests/5/approvals"*) echo '{"approved":true,"approved_by":[{"user":{"username":"bo"}}]}';;
*"merge_requests/5/pipelines"*) echo '[]';;
*"merge_requests/5/diffs"*) printf '%s' '[{"new_path":"a.go","diff":"@@ -1 +1,2 @@\n-old\n+new\n+more\n"}]';;
*"merge_requests/5/commits"*) echo '[{"id":"abcdef123","title":"do it","author_name":"Ana"}]';;
*"merge_requests/5/notes"*) echo '[{"body":"assigned to @bo","system":true,"author":{"username":"x"}},{"body":"why?","author":{"username":"cy"}}]';;
*"merge_requests/5"*) echo '{"iid":5,"title":"T","state":"opened","labels":["ui"],"user_notes_count":1,"reviewers":[{"username":"bo"},{"username":"cy"}],"changes_count":"1"}';;
esac`)
	mr, err := gitlab{}.ViewMR(&Repo{Root: t.TempDir(), Slug: "o/n"}, "5")
	if err != nil || mr.Review != "approved" || mr.Additions != 2 || mr.Deletions != 1 || len(mr.FileList) != 1 || mr.Files != 1 {
		t.Fatalf("%+v %v", mr, err)
	}
	if len(mr.Reviewers) != 2 || mr.Reviewers[0].State != "approved" || mr.Reviewers[1].State != "pending" {
		t.Errorf("reviewers = %+v", mr.Reviewers)
	}
	if len(mr.Notes) != 1 || mr.Notes[0].Author != "cy" || len(mr.CommitList) != 1 || mr.Labels[0] != "ui" {
		t.Errorf("what GitLab says itself is not a comment: %+v %+v", mr.Notes, mr.CommitList)
	}
}

func TestGitLabJobsGoInTheOrderOfThePipelineWithTheirNeeds(t *testing.T) {
	fakeTool(t, "glab", `case "$*" in
*graphql*) echo '{"data":{"project":{"pipeline":{"jobs":{"nodes":[{"name":"deploy","needs":{"nodes":[{"name":"build"}]}},{"name":"build","needs":{"nodes":[]}}]}}}}}';;
*"/pipelines/9/jobs"*) echo '[{"id":73,"name":"deploy","status":"manual","stage":"deploy"},{"id":71,"name":"build","status":"success","stage":"build","started_at":"2026-10-26T10:00:00Z","finished_at":"2026-10-26T10:00:30Z"},{"id":72,"name":"test","status":"failed","stage":"test"}]';;
esac`)
	jobs, err := gitlab{}.Jobs(&Repo{Root: t.TempDir(), Slug: "o/n"}, "9")
	if err != nil || len(jobs) != 3 || jobs[0].Name != "build" || jobs[2].Name != "deploy" || jobs[0].Duration() != 30*time.Second {
		t.Fatalf("%+v %v", jobs, err)
	}
	if strings.Join(jobs[2].Needs, ",") != "build" || jobs[1].Needs != nil || jobs[0].Stage != "build" {
		t.Fatalf("needs = %+v", jobs)
	}
}

func TestGitLabLogIsTheTraceOfTheFailedJobs(t *testing.T) {
	calls := fakeTool(t, "glab", `case "$*" in
*"/jobs/72/trace"*) printf '\033[31;1mERROR: job failed\033[0m\nexit code 1\n';;
*"/pipelines/9/jobs"*) cat "$D/gl-jobs.json";;
esac`)
	log, err := gitlab{}.Log(&Repo{Root: t.TempDir(), Slug: "o/n"}, "9", true)
	if err != nil || !strings.Contains(log, "── test") || !strings.Contains(log, "ERROR: job failed") || strings.Contains(log, "\x1b") {
		t.Fatalf("log = %q %v", log, err)
	}
	for _, c := range calls() {
		if strings.Contains(c, "/jobs/71/trace") {
			t.Fatal("a job that passed has no business in the log of failures")
		}
	}
}

func TestGitLabActionsRunTheRightCommands(t *testing.T) {
	calls := fakeTool(t, "glab", "")
	repo := &Repo{Root: t.TempDir(), Slug: "owner/name"}
	g := gitlab{}
	for want, do := range map[string]func() error{
		"mr approve 5 -R owner/name":                             func() error { return g.Approve(repo, "5") },
		"mr close 5 -R owner/name":                               func() error { return g.Close(repo, "5") },
		"mr checkout 5 -R owner/name":                            func() error { return g.Checkout(repo, "5") },
		"mr merge 5 --yes --squash -R owner/name":                func() error { return g.Merge(repo, "5", "squash") },
		"mr merge 5 --yes --rebase -R owner/name":                func() error { return g.Merge(repo, "5", "rebase") },
		"mr merge 5 --yes -R owner/name":                         func() error { return g.Merge(repo, "5", "merge") },
		"api -X POST projects/owner%2Fname/pipelines/9/retry":    func() error { return g.Rerun(repo, "9", true) },
		"api -X POST projects/owner%2Fname/pipelines/9/cancel":   func() error { return g.Cancel(repo, "9") },
		"mr note 5 -m hi there -R owner/name":                    func() error { return g.Comment(repo, "5", "hi there") },
		"mr update 5 --draft -R owner/name":                      func() error { return g.SetDraft(repo, "5", true) },
		"mr update 5 --ready -R owner/name":                      func() error { return g.SetDraft(repo, "5", false) },
		"api -X POST projects/owner%2Fname/pipeline?ref=fix%2Fa": func() error { return g.Start(repo, "pipeline", "fix/a") },
	} {
		if err := do(); err != nil {
			t.Errorf("%s: %v", want, err)
			continue
		}
		if got := last(calls()); got != want {
			t.Errorf("ran %q, want %q", got, want)
		}
	}
}

func TestPlainDropsColourCodesAndTailKeepsTheEnd(t *testing.T) {
	if got := plain("\x1b[0K\x1b[32;1mok\x1b[0;m\r\nnext"); got != "ok\nnext" {
		t.Fatalf("plain = %q", got)
	}
	lines := tail("a\nb\nc\nd\n", 2)
	if len(lines) != 2 || lines[0] != "c" || lines[1] != "d" {
		t.Fatalf("tail = %v", lines)
	}
}
