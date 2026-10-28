package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

type github struct{}

func (github) Kind() string   { return "github" }
func (github) Prefix() string { return "#" }

const ghMRFields = "number,title,author,isDraft,state,headRefName,baseRefName,reviewDecision,statusCheckRollup,url,updatedAt,mergeable," +
	"createdAt,additions,deletions,changedFiles,labels,assignees,milestone,autoMergeRequest"

// ghViewFields are what only the detail asks for: they cost more
const ghViewFields = ",body,files,commits,comments,latestReviews,reviewRequests"

type ghUser struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

type ghMR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	IsDraft        bool      `json:"isDraft"`
	State          string    `json:"state"`
	HeadRefName    string    `json:"headRefName"`
	BaseRefName    string    `json:"baseRefName"`
	ReviewDecision string    `json:"reviewDecision"`
	URL            string    `json:"url"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Mergeable      string    `json:"mergeable"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	ChangedFiles   int       `json:"changedFiles"`
	Labels         []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []ghUser `json:"assignees"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	AutoMerge *struct{} `json:"autoMergeRequest"`
	Files     []struct {
		Path      string `json:"path"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	} `json:"files"`
	Commits []struct {
		OID      string    `json:"oid"`
		Headline string    `json:"messageHeadline"`
		Authored time.Time `json:"authoredDate"`
		Authors  []ghUser  `json:"authors"`
	} `json:"commits"`
	Comments []struct {
		Author    ghUser    `json:"author"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"comments"`
	LatestReviews []struct {
		Author ghUser `json:"author"`
		State  string `json:"state"`
	} `json:"latestReviews"`
	ReviewRequests []struct {
		Login string `json:"login"`
		Name  string `json:"name"`
		Slug  string `json:"slug"`
	} `json:"reviewRequests"`
	Checks []struct {
		Type       string `json:"__typename"`
		Name       string `json:"name"`
		Context    string `json:"context"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	} `json:"statusCheckRollup"`
}

// ghCheckStatus maps the status of a CheckRun or of a StatusContext to pass | fail | run | wait | skip.
func ghCheckStatus(typ, status, conclusion, state string) string {
	if typ == "StatusContext" {
		switch strings.ToUpper(state) {
		case "SUCCESS":
			return "pass"
		case "PENDING", "EXPECTED":
			return "run"
		}
		return "fail"
	}
	switch strings.ToUpper(status) {
	case "COMPLETED":
	case "QUEUED", "WAITING", "PENDING", "REQUESTED":
		return "wait"
	default:
		return "run"
	}
	switch strings.ToUpper(conclusion) {
	case "SUCCESS":
		return "pass"
	case "NEUTRAL", "SKIPPED":
		return "skip"
	}
	return "fail"
}

func (m ghMR) toMR() MR {
	mr := MR{
		ID: fmt.Sprint(m.Number), Title: m.Title, Author: m.Author.Login, Branch: m.HeadRefName, Target: m.BaseRefName,
		URL: m.URL, Body: m.Body, Created: m.CreatedAt, Updated: m.UpdatedAt, Mergeable: map[string]string{"MERGEABLE": "ready", "CONFLICTING": "conflicts"}[strings.ToUpper(m.Mergeable)],
		Additions: m.Additions, Deletions: m.Deletions, Files: m.ChangedFiles, Comments: len(m.Comments), AutoMerge: m.AutoMerge != nil,
	}
	for _, l := range m.Labels {
		mr.Labels = append(mr.Labels, l.Name)
	}
	for _, u := range m.Assignees {
		mr.Assignees = append(mr.Assignees, u.Login)
	}
	if m.Milestone != nil {
		mr.Milestone = m.Milestone.Title
	}
	for _, f := range m.Files {
		mr.FileList = append(mr.FileList, File{Path: f.Path, Additions: f.Additions, Deletions: f.Deletions})
	}
	for _, c := range m.Commits {
		author := ""
		if len(c.Authors) > 0 {
			author = cmp.Or(c.Authors[0].Login, c.Authors[0].Name)
		}
		mr.CommitList = append(mr.CommitList, Commit{SHA: c.OID, Title: c.Headline, Author: author, When: c.Authored})
	}
	for _, c := range m.Comments {
		mr.Notes = append(mr.Notes, Note{Author: c.Author.Login, Body: c.Body, When: c.CreatedAt})
	}
	// who reviewed, and then who is asked to and has not yet
	seen := map[string]bool{}
	for _, r := range m.LatestReviews {
		st := map[string]string{"APPROVED": "approved", "CHANGES_REQUESTED": "changes", "COMMENTED": "commented", "DISMISSED": "pending"}[strings.ToUpper(r.State)]
		mr.Reviewers = append(mr.Reviewers, Reviewer{Name: r.Author.Login, State: cmp.Or(st, "pending")})
		seen[r.Author.Login] = true
	}
	for _, r := range m.ReviewRequests {
		name := cmp.Or(r.Login, r.Slug, r.Name)
		if !seen[name] {
			mr.Reviewers = append(mr.Reviewers, Reviewer{Name: name, State: "pending"})
		}
	}
	switch strings.ToUpper(m.State) {
	case "MERGED":
		mr.State = "merged"
	case "CLOSED":
		mr.State = "closed"
	default:
		mr.State = "open"
		if m.IsDraft {
			mr.State = "draft"
		}
	}
	switch strings.ToUpper(m.ReviewDecision) {
	case "APPROVED":
		mr.Review = "approved"
	case "CHANGES_REQUESTED":
		mr.Review = "changes"
	case "REVIEW_REQUIRED":
		mr.Review = "review"
	}
	for _, c := range m.Checks {
		name := c.Name
		if name == "" {
			name = c.Context
		}
		mr.CheckList = append(mr.CheckList, Check{Name: name, Status: ghCheckStatus(c.Type, c.Status, c.Conclusion, c.State)})
	}
	mr.Checks = overall(mr.CheckList)
	return mr
}

// overall folds the checks into one: any failure fails, anything pending is running, else it passes.
func overall(checks []Check) string {
	pass, running := 0, false
	for _, c := range checks {
		switch c.Status {
		case "fail":
			return "fail"
		case "run", "wait":
			running = true
		case "pass":
			pass++
		}
	}
	switch {
	case running:
		return "run"
	case pass > 0:
		return "pass"
	}
	return ""
}

func ghJSON(r *Repo, v any, args ...string) error {
	out, err := run(r.Root, "gh", append(args, "-R", r.Slug)...)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

func (github) ListMRs(r *Repo) ([]MR, error) {
	var list []ghMR
	if err := ghJSON(r, &list, "pr", "list", "--state", "open", "--limit", "50", "--json", ghMRFields); err != nil {
		return nil, err
	}
	out := make([]MR, len(list))
	for i, m := range list {
		out[i] = m.toMR()
	}
	return out, nil
}

func (github) ViewMR(r *Repo, id string) (MR, error) {
	var m ghMR
	if err := ghJSON(r, &m, "pr", "view", id, "--json", ghMRFields+ghViewFields); err != nil {
		return MR{}, err
	}
	return m.toMR(), nil
}

type ghRun struct {
	ID         int64     `json:"databaseId"`
	SHA        string    `json:"headSha"`
	Name       string    `json:"name"`
	Title      string    `json:"displayTitle"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Branch     string    `json:"headBranch"`
	Event      string    `json:"event"`
	URL        string    `json:"url"`
	Created    time.Time `json:"createdAt"`
}

// runStatus maps the status and the conclusion of GitHub to success | failure | running | queued | cancelled | skipped.
func ghRunStatus(status, conclusion string) string {
	switch strings.ToLower(status) {
	case "completed":
		switch strings.ToLower(conclusion) {
		case "success":
			return "success"
		case "cancelled":
			return "cancelled"
		case "skipped", "neutral":
			return "skipped"
		}
		return "failure"
	case "in_progress":
		return "running"
	}
	return "queued"
}

func (github) ListRuns(r *Repo, branch string) ([]Run, error) {
	args := []string{"run", "list", "--limit", "30", "--json", "databaseId,name,displayTitle,status,conclusion,headBranch,event,createdAt,url,headSha"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	var list []ghRun
	if err := ghJSON(r, &list, args...); err != nil {
		return nil, err
	}
	out := make([]Run, len(list))
	for i, x := range list {
		out[i] = Run{ID: fmt.Sprint(x.ID), Name: x.Name, Title: x.Title, Branch: x.Branch, Event: x.Event, URL: x.URL, Created: x.Created,
			Status: ghRunStatus(x.Status, x.Conclusion), SHA: x.SHA}
	}
	return out, nil
}

func (github) Jobs(r *Repo, runID string) ([]Job, error) {
	var res struct {
		Jobs []struct {
			ID          int64     `json:"databaseId"`
			Name        string    `json:"name"`
			Status      string    `json:"status"`
			Conclusion  string    `json:"conclusion"`
			StartedAt   time.Time `json:"startedAt"`
			CompletedAt time.Time `json:"completedAt"`
			Steps       []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := ghJSON(r, &res, "run", "view", runID, "--json", "jobs"); err != nil {
		return nil, err
	}
	out := make([]Job, len(res.Jobs))
	for i, j := range res.Jobs {
		out[i] = Job{ID: fmt.Sprint(j.ID), Name: j.Name, Status: ghRunStatus(j.Status, j.Conclusion), Started: j.StartedAt, Finished: j.CompletedAt}
		for _, st := range j.Steps {
			out[i].Steps = append(out[i].Steps, Step{Name: st.Name, Status: ghRunStatus(st.Status, st.Conclusion)})
		}
	}
	// what each job needs is in the workflow, as it was at the commit of the run; without it the graph is flat
	if needs, err := ghNeeds(r, runID); err == nil {
		linkNeeds(out, needs)
	}
	return out, nil
}

// ghWorkflows caches the jobs of a workflow file at a commit, which never change: "<slug>|<path>@<sha>".
var ghWorkflows sync.Map

// ghNeeds reads the workflow of a run: for each of its jobs, its name and the ids of the jobs it needs.
func ghNeeds(r *Repo, runID string) (map[string]workflowJob, error) {
	out, err := run(r.Root, "gh", "api", "repos/"+r.Slug+"/actions/runs/"+runID)
	if err != nil {
		return nil, err
	}
	var info struct {
		Path string `json:"path"`
		SHA  string `json:"head_sha"`
	}
	if err := json.Unmarshal(out, &info); err != nil || info.Path == "" {
		return nil, fmt.Errorf("the run does not say its workflow")
	}
	path, _, _ := strings.Cut(info.Path, "@")
	key := r.Slug + "|" + path + "@" + info.SHA
	if v, ok := ghWorkflows.Load(key); ok {
		return v.(map[string]workflowJob), nil
	}
	raw, err := run(r.Root, "gh", "api", "-H", "Accept: application/vnd.github.raw", "repos/"+r.Slug+"/contents/"+path+"?ref="+info.SHA)
	if err != nil {
		return nil, err
	}
	jobs, err := parseWorkflow(raw)
	if err != nil {
		return nil, err
	}
	ghWorkflows.Store(key, jobs)
	return jobs, nil
}
func (github) Log(r *Repo, runID string, failedOnly bool) (string, error) {
	flag := "--log"
	if failedOnly {
		flag = "--log-failed"
	}
	out, err := run(r.Root, "gh", "run", "view", runID, flag, "-R", r.Slug)
	return ghLog(string(out)), err
}

var logStamp = regexp.MustCompile(`^\x{FEFF}?\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z ?`)

// ghLog is the log as gh prints it ("<job>\t<step>\t<time> <text>" a line) made readable: a heading where the job
// or the step changes, then the text alone.
func ghLog(out string) string {
	var b strings.Builder
	last := ""
	for _, line := range strings.Split(out, "\n") {
		job, rest, ok := strings.Cut(line, "\t")
		step, text, ok2 := strings.Cut(rest, "\t")
		if !ok || !ok2 {
			b.WriteString(line + "\n")
			continue
		}
		head := job
		if step != "" && step != "UNKNOWN STEP" {
			head += " › " + step
		}
		if head != last {
			b.WriteString("── " + head + "\n")
			last = head
		}
		b.WriteString(logStamp.ReplaceAllString(text, "") + "\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func ghDo(r *Repo, args ...string) error {
	_, err := run(r.Root, "gh", append(args, "-R", r.Slug)...)
	return err
}

func (github) Approve(r *Repo, id string) error { return ghDo(r, "pr", "review", id, "--approve") }
func (github) Close(r *Repo, id string) error   { return ghDo(r, "pr", "close", id) }
func (github) Checkout(r *Repo, id string) error {
	return ghDo(r, "pr", "checkout", id)
}

func (github) Merge(r *Repo, id, method string) error {
	switch method {
	case "squash", "merge", "rebase":
		return ghDo(r, "pr", "merge", id, "--"+method)
	}
	return fmt.Errorf("unknown merge method %q", method)
}

func (github) Rerun(r *Repo, runID string, failedOnly bool) error {
	if failedOnly {
		return ghDo(r, "run", "rerun", runID, "--failed")
	}
	return ghDo(r, "run", "rerun", runID)
}

func (github) Cancel(r *Repo, runID string) error { return ghDo(r, "run", "cancel", runID) }

func (github) Comment(r *Repo, id, body string) error {
	return ghDo(r, "pr", "comment", id, "--body", body)
}

func (github) SetDraft(r *Repo, id string, draft bool) error {
	if draft {
		return ghDo(r, "pr", "ready", id, "--undo")
	}
	return ghDo(r, "pr", "ready", id)
}

func (github) Workflows(r *Repo) ([]Workflow, error) {
	var list []struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Path  string `json:"path"`
		State string `json:"state"`
	}
	if err := ghJSON(r, &list, "workflow", "list", "--json", "id,name,path,state"); err != nil {
		return nil, err
	}
	var out []Workflow
	for _, w := range list {
		// the dynamic ones (Dependabot, Copilot…) are GitHub's own: they cannot be started by hand
		if w.State == "active" && !strings.HasPrefix(w.Path, "dynamic/") {
			out = append(out, Workflow{ID: filepathBase(w.Path), Name: w.Name + " (" + filepathBase(w.Path) + ")"})
		}
	}
	return out, nil
}

// Start runs a workflow (its file) on a branch; it must say workflow_dispatch in its on:, which gh reports when it does not.
func (github) Start(r *Repo, workflow, branch string) error {
	if workflow == "" {
		return fmt.Errorf("which workflow?")
	}
	return ghDo(r, "workflow", "run", workflow, "--ref", branch)
}

func (github) Branches(r *Repo) ([]string, error) {
	out, err := run(r.Root, "gh", "api", "repos/"+r.Slug+"/branches?per_page=100")
	if err != nil {
		return nil, err
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	names := make([]string, len(list))
	for i, b := range list {
		names[i] = b.Name
	}
	return names, nil
}

func filepathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
