package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type github struct{}

func (github) Kind() string   { return "github" }
func (github) Prefix() string { return "#" }

const ghMRFields = "number,title,author,isDraft,state,headRefName,baseRefName,reviewDecision,statusCheckRollup,url,updatedAt,mergeable"

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
	UpdatedAt      time.Time `json:"updatedAt"`
	Mergeable      string    `json:"mergeable"`
	Checks         []struct {
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
		URL: m.URL, Body: m.Body, Updated: m.UpdatedAt, Mergeable: map[string]string{"MERGEABLE": "ready", "CONFLICTING": "conflicts"}[strings.ToUpper(m.Mergeable)],
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
	if err := ghJSON(r, &m, "pr", "view", id, "--json", ghMRFields+",body"); err != nil {
		return MR{}, err
	}
	return m.toMR(), nil
}

type ghRun struct {
	ID         int64     `json:"databaseId"`
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
	args := []string{"run", "list", "--limit", "30", "--json", "databaseId,name,displayTitle,status,conclusion,headBranch,event,createdAt,url"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	var list []ghRun
	if err := ghJSON(r, &list, args...); err != nil {
		return nil, err
	}
	out := make([]Run, len(list))
	for i, x := range list {
		out[i] = Run{ID: fmt.Sprint(x.ID), Name: x.Name, Title: x.Title, Branch: x.Branch, Event: x.Event, URL: x.URL, Created: x.Created, Status: ghRunStatus(x.Status, x.Conclusion)}
	}
	return out, nil
}

func (github) Jobs(r *Repo, runID string) ([]Job, error) {
	var res struct {
		Jobs []struct {
			ID         int64  `json:"databaseId"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	if err := ghJSON(r, &res, "run", "view", runID, "--json", "jobs"); err != nil {
		return nil, err
	}
	out := make([]Job, len(res.Jobs))
	for i, j := range res.Jobs {
		out[i] = Job{ID: fmt.Sprint(j.ID), Name: j.Name, Status: ghRunStatus(j.Status, j.Conclusion)}
	}
	return out, nil
}

func (github) Log(r *Repo, runID string, failedOnly bool) (string, error) {
	flag := "--log"
	if failedOnly {
		flag = "--log-failed"
	}
	out, err := run(r.Root, "gh", "run", "view", runID, flag, "-R", r.Slug)
	return string(out), err
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
