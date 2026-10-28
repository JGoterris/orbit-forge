package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// gitlab talks to GitLab through glab. Reads go through `glab api` (the REST API), which is what glab keeps stable;
// the actions use its own commands.
type gitlab struct{}

func (gitlab) Kind() string   { return "gitlab" }
func (gitlab) Prefix() string { return "!" }

func project(r *Repo) string { return "projects/" + url.PathEscape(r.Slug) }

func glJSON(r *Repo, v any, args ...string) error {
	out, err := run(r.Root, "glab", append([]string{"api"}, args...)...)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, v)
}

type glMR struct {
	IID         int    `json:"iid"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Author      struct {
		Username string `json:"username"`
	} `json:"author"`
	Draft        bool      `json:"draft"`
	State        string    `json:"state"`
	SourceBranch string    `json:"source_branch"`
	TargetBranch string    `json:"target_branch"`
	WebURL       string    `json:"web_url"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MergeStatus  string    `json:"detailed_merge_status"`
	Labels       []string  `json:"labels"`
	Notes        int       `json:"user_notes_count"`
	Assignees    []glUser  `json:"assignees"`
	Reviewers    []glUser  `json:"reviewers"`
	Milestone    *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	AutoMerge    bool   `json:"merge_when_pipeline_succeeds"`
	ChangesCount string `json:"changes_count"`
	HeadPipeline *struct {
		Status string `json:"status"`
	} `json:"head_pipeline"`
}

type glUser struct {
	Username string `json:"username"`
}

// glStatus maps the status of a pipeline or a job to success | failure | running | queued | cancelled | skipped.
func glStatus(s string) string {
	switch s {
	case "success", "passed":
		return "success"
	case "failed":
		return "failure"
	case "running":
		return "running"
	case "canceled", "canceling", "cancelled":
		return "cancelled"
	case "skipped":
		return "skipped"
	}
	return "queued"
}

// glMergeable is what GitLab says about merging, read: "mergeable" is ready, the rest is the reason it is not.
func glMergeable(status string) string {
	switch status {
	case "mergeable":
		return "ready"
	case "checking", "unchecked", "preparing", "":
		return ""
	}
	return strings.ReplaceAll(status, "_", " ")
}

func (m glMR) toMR() MR {
	mr := MR{
		ID: fmt.Sprint(m.IID), Title: m.Title, Author: m.Author.Username, Branch: m.SourceBranch, Target: m.TargetBranch,
		URL: m.WebURL, Body: m.Description, Created: m.CreatedAt, Updated: m.UpdatedAt, Mergeable: glMergeable(m.MergeStatus),
		Labels: m.Labels, Comments: m.Notes, AutoMerge: m.AutoMerge,
	}
	mr.Files, _ = strconv.Atoi(strings.TrimSuffix(m.ChangesCount, "+"))
	for _, u := range m.Assignees {
		mr.Assignees = append(mr.Assignees, u.Username)
	}
	for _, u := range m.Reviewers {
		mr.Reviewers = append(mr.Reviewers, Reviewer{Name: u.Username, State: "pending"})
	}
	if m.Milestone != nil {
		mr.Milestone = m.Milestone.Title
	}
	switch m.State {
	case "merged":
		mr.State = "merged"
	case "closed":
		mr.State = "closed"
	default:
		mr.State = "open"
		if m.Draft {
			mr.State = "draft"
		}
	}
	if m.HeadPipeline != nil {
		switch glStatus(m.HeadPipeline.Status) {
		case "success":
			mr.Checks = "pass"
		case "failure":
			mr.Checks = "fail"
		case "running", "queued":
			mr.Checks = "run"
		}
	}
	return mr
}

func (gitlab) ListMRs(r *Repo) ([]MR, error) {
	var list []glMR
	if err := glJSON(r, &list, project(r)+"/merge_requests?state=opened&per_page=50&order_by=updated_at"); err != nil {
		return nil, err
	}
	out := make([]MR, len(list))
	for i, m := range list {
		out[i] = m.toMR()
	}
	return out, nil
}

func (g gitlab) ViewMR(r *Repo, id string) (MR, error) {
	var m glMR
	base := project(r) + "/merge_requests/" + id
	if err := glJSON(r, &m, base); err != nil {
		return MR{}, err
	}
	mr := m.toMR()
	// the rest is more of the same merge request: what fails to come is left out, not an error
	var ap struct {
		Approved   bool `json:"approved"`
		ApprovedBy []struct {
			User glUser `json:"user"`
		} `json:"approved_by"`
	}
	if glJSON(r, &ap, base+"/approvals") == nil {
		if ap.Approved {
			mr.Review = "approved"
		}
		for _, a := range ap.ApprovedBy {
			found := false
			for i := range mr.Reviewers {
				if mr.Reviewers[i].Name == a.User.Username {
					mr.Reviewers[i].State, found = "approved", true
				}
			}
			if !found {
				mr.Reviewers = append(mr.Reviewers, Reviewer{Name: a.User.Username, State: "approved"})
			}
		}
	}
	var pipes []struct {
		ID int `json:"id"`
	}
	if glJSON(r, &pipes, base+"/pipelines?per_page=1") == nil && len(pipes) > 0 {
		if jobs, err := g.Jobs(r, fmt.Sprint(pipes[0].ID)); err == nil {
			for _, j := range jobs {
				st := map[string]string{"success": "pass", "failure": "fail", "running": "run", "queued": "wait", "skipped": "skip", "cancelled": "fail"}[j.Status]
				mr.CheckList = append(mr.CheckList, Check{Name: j.Name, Status: st})
			}
		}
	}
	var diffs []struct {
		NewPath string `json:"new_path"`
		Diff    string `json:"diff"`
	}
	if glJSON(r, &diffs, base+"/diffs?per_page=100") == nil {
		mr.Additions, mr.Deletions = 0, 0
		for _, d := range diffs {
			f := File{Path: d.NewPath}
			for _, l := range strings.Split(d.Diff, "\n") {
				switch {
				case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
				case strings.HasPrefix(l, "+"):
					f.Additions++
				case strings.HasPrefix(l, "-"):
					f.Deletions++
				}
			}
			mr.Additions += f.Additions
			mr.Deletions += f.Deletions
			mr.FileList = append(mr.FileList, f)
		}
	}
	var commits []struct {
		ID         string    `json:"id"`
		Title      string    `json:"title"`
		AuthorName string    `json:"author_name"`
		Created    time.Time `json:"created_at"`
	}
	if glJSON(r, &commits, base+"/commits?per_page=100") == nil {
		for _, c := range commits {
			mr.CommitList = append(mr.CommitList, Commit{SHA: c.ID, Title: c.Title, Author: c.AuthorName, When: c.Created})
		}
	}
	var notes []struct {
		Body    string    `json:"body"`
		Author  glUser    `json:"author"`
		Created time.Time `json:"created_at"`
		System  bool      `json:"system"`
	}
	if glJSON(r, &notes, base+"/notes?sort=asc&order_by=created_at&per_page=100") == nil {
		for _, n := range notes {
			if !n.System {
				mr.Notes = append(mr.Notes, Note{Author: n.Author.Username, Body: n.Body, When: n.Created})
			}
		}
	}
	return mr, nil
}

func (gitlab) ListRuns(r *Repo, branch string) ([]Run, error) {
	path := project(r) + "/pipelines?per_page=30"
	if branch != "" {
		path += "&ref=" + url.QueryEscape(branch)
	}
	var list []struct {
		ID        int       `json:"id"`
		Status    string    `json:"status"`
		Ref       string    `json:"ref"`
		Source    string    `json:"source"`
		CreatedAt time.Time `json:"created_at"`
		WebURL    string    `json:"web_url"`
	}
	if err := glJSON(r, &list, path); err != nil {
		return nil, err
	}
	out := make([]Run, len(list))
	for i, p := range list {
		out[i] = Run{ID: fmt.Sprint(p.ID), Name: fmt.Sprintf("pipeline #%d", p.ID), Title: p.Ref, Branch: p.Ref, Event: p.Source,
			URL: p.WebURL, Created: p.CreatedAt, Status: glStatus(p.Status)}
	}
	return out, nil
}

type glJob struct {
	ID         int        `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Stage      string     `json:"stage"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

func (gitlab) Jobs(r *Repo, runID string) ([]Job, error) {
	var list []glJob
	if err := glJSON(r, &list, project(r)+"/pipelines/"+runID+"/jobs?per_page=100"); err != nil {
		return nil, err
	}
	// in the order the pipeline made them, whatever order the list comes in
	slices.SortFunc(list, func(a, b glJob) int { return a.ID - b.ID })
	out := make([]Job, len(list))
	for i, j := range list {
		out[i] = Job{ID: fmt.Sprint(j.ID), Name: j.Name, Status: glStatus(j.Status), Stage: j.Stage}
		if j.StartedAt != nil {
			out[i].Started = *j.StartedAt
		}
		if j.FinishedAt != nil {
			out[i].Finished = *j.FinishedAt
		}
	}
	if needs, err := glNeeds(r, runID); err == nil {
		for i := range out {
			if n, ok := needs[out[i].Name]; ok {
				out[i].Needs = n
			}
		}
	}
	return out, nil
}

// glNeeds asks GraphQL, the only one that says it, for the needs: of the jobs of a pipeline. A job that has none
// is not in the map: it waits for the stage before its own.
func glNeeds(r *Repo, pipelineID string) (map[string][]string, error) {
	query := `query($path: ID!, $id: CiPipelineID!) { project(fullPath: $path) { pipeline(id: $id) { jobs(first: 100) { nodes { name needs { nodes { name } } } } } } }`
	out, err := run(r.Root, "glab", "api", "graphql", "-f", "query="+query, "-f", "path="+r.Slug, "-f", "id=gid://gitlab/Ci::Pipeline/"+pipelineID)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data struct {
			Project struct {
				Pipeline struct {
					Jobs struct {
						Nodes []struct {
							Name  string `json:"name"`
							Needs struct {
								Nodes []struct {
									Name string `json:"name"`
								} `json:"nodes"`
							} `json:"needs"`
						} `json:"nodes"`
					} `json:"jobs"`
				} `json:"pipeline"`
			} `json:"project"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	needs := map[string][]string{}
	for _, j := range res.Data.Project.Pipeline.Jobs.Nodes {
		for _, n := range j.Needs.Nodes {
			needs[j.Name] = append(needs[j.Name], n.Name)
		}
	}
	return needs, nil
}

func (g gitlab) Log(r *Repo, runID string, failedOnly bool) (string, error) {
	jobs, err := g.Jobs(r, runID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, j := range jobs {
		if failedOnly && j.Status != "failure" {
			continue
		}
		trace, err := run(r.Root, "glab", "api", project(r)+"/jobs/"+j.ID+"/trace")
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "── %s\n%s\n", j.Name, strings.Join(tail(string(trace), 60), "\n"))
	}
	return b.String(), nil
}

func glDo(r *Repo, args ...string) error {
	_, err := run(r.Root, "glab", append(args, "-R", r.Slug)...)
	return err
}

func (gitlab) Approve(r *Repo, id string) error  { return glDo(r, "mr", "approve", id) }
func (gitlab) Close(r *Repo, id string) error    { return glDo(r, "mr", "close", id) }
func (gitlab) Checkout(r *Repo, id string) error { return glDo(r, "mr", "checkout", id) }

func (gitlab) Merge(r *Repo, id, method string) error {
	args := []string{"mr", "merge", id, "--yes"}
	switch method {
	case "squash":
		args = append(args, "--squash")
	case "rebase":
		args = append(args, "--rebase")
	case "merge":
	default:
		return fmt.Errorf("unknown merge method %q", method)
	}
	return glDo(r, args...)
}

func glPost(r *Repo, path string) error {
	_, err := run(r.Root, "glab", "api", "-X", "POST", project(r)+path)
	return err
}

func (gitlab) Rerun(r *Repo, runID string, _ bool) error {
	return glPost(r, "/pipelines/"+runID+"/retry")
}
func (gitlab) Cancel(r *Repo, runID string) error { return glPost(r, "/pipelines/"+runID+"/cancel") }

func (gitlab) Comment(r *Repo, id, body string) error { return glDo(r, "mr", "note", id, "-m", body) }

func (gitlab) SetDraft(r *Repo, id string, draft bool) error {
	if draft {
		return glDo(r, "mr", "update", id, "--draft")
	}
	return glDo(r, "mr", "update", id, "--ready")
}

// Workflows: a GitLab project has one pipeline, which .gitlab-ci.yml makes of the branch it runs on.
func (gitlab) Workflows(*Repo) ([]Workflow, error) {
	return []Workflow{{ID: "pipeline", Name: "pipeline"}}, nil
}

func (gitlab) Start(r *Repo, _, branch string) error {
	_, err := run(r.Root, "glab", "api", "-X", "POST", project(r)+"/pipeline?ref="+url.QueryEscape(branch))
	return err
}

func (gitlab) Branches(r *Repo) ([]string, error) {
	var list []struct {
		Name string `json:"name"`
	}
	if err := glJSON(r, &list, project(r)+"/repository/branches?per_page=100"); err != nil {
		return nil, err
	}
	names := make([]string, len(list))
	for i, b := range list {
		names[i] = b.Name
	}
	return names, nil
}
