package main

import (
	"encoding/json"
	"fmt"
	"net/url"
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
	UpdatedAt    time.Time `json:"updated_at"`
	MergeStatus  string    `json:"detailed_merge_status"`
	HeadPipeline *struct {
		Status string `json:"status"`
	} `json:"head_pipeline"`
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
		URL: m.WebURL, Body: m.Description, Updated: m.UpdatedAt, Mergeable: glMergeable(m.MergeStatus),
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
	if err := glJSON(r, &m, project(r)+"/merge_requests/"+id); err != nil {
		return MR{}, err
	}
	mr := m.toMR()
	var ap struct {
		Approved bool `json:"approved"`
	}
	if glJSON(r, &ap, project(r)+"/merge_requests/"+id+"/approvals") == nil && ap.Approved {
		mr.Review = "approved"
	}
	var pipes []struct {
		ID int `json:"id"`
	}
	if glJSON(r, &pipes, project(r)+"/merge_requests/"+id+"/pipelines?per_page=1") == nil && len(pipes) > 0 {
		if jobs, err := g.Jobs(r, fmt.Sprint(pipes[0].ID)); err == nil {
			for _, j := range jobs {
				st := map[string]string{"success": "pass", "failure": "fail", "running": "run", "queued": "wait", "skipped": "skip", "cancelled": "fail"}[j.Status]
				mr.CheckList = append(mr.CheckList, Check{Name: j.Name, Status: st})
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

func (gitlab) Jobs(r *Repo, runID string) ([]Job, error) {
	var list []struct {
		ID     int    `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := glJSON(r, &list, project(r)+"/pipelines/"+runID+"/jobs?per_page=100"); err != nil {
		return nil, err
	}
	out := make([]Job, len(list))
	for i, j := range list {
		out[i] = Job{ID: fmt.Sprint(j.ID), Name: j.Name, Status: glStatus(j.Status)}
	}
	return out, nil
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
