package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type MR struct {
	ID        string
	Title     string
	Author    string
	Branch    string
	Target    string
	URL       string
	Body      string
	State     string // open | draft | merged | closed
	Review    string // approved | changes | review | ""
	Checks    string // pass | fail | run | ""
	Mergeable string
	Updated   time.Time
	CheckList []Check
}

type Check struct {
	Name   string
	Status string // pass | fail | run | wait | skip
}

// Run is a run of CI: a workflow run on GitHub, a pipeline on GitLab.
type Run struct {
	ID      string
	Name    string
	Title   string
	Branch  string
	Event   string
	Status  string // success | failure | running | queued | cancelled | skipped
	URL     string
	Created time.Time
}

type Job struct {
	ID     string
	Name   string
	Status string
}

// Forge is a place that hosts merge requests and CI, reached through its command line tool.
type Forge interface {
	Kind() string   // github | gitlab
	Prefix() string // "#" | "!": how its merge requests are numbered
	ListMRs(r *Repo) ([]MR, error)
	ViewMR(r *Repo, id string) (MR, error)
	// ListRuns are the latest runs of a branch ("" for all of them)
	ListRuns(r *Repo, branch string) ([]Run, error)
	Jobs(r *Repo, runID string) ([]Job, error)
	// Log is the output of the jobs of a run, the failed ones only when failedOnly
	Log(r *Repo, runID string, failedOnly bool) (string, error)
	Approve(r *Repo, id string) error
	Merge(r *Repo, id, method string) error
	Close(r *Repo, id string) error
	Checkout(r *Repo, id string) error
	Rerun(r *Repo, runID string, failedOnly bool) error
	Cancel(r *Repo, runID string) error
}

type Repo struct {
	Root     string
	Branch   string
	Slug     string // owner/name
	Forge    Forge
	Services []string
}

var errNoRepo = errors.New("not in a git repository")

var scpRemote = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+?)(?:\.git)?/?$`)

// parseRemote reads the host and the owner/name of a remote url: https://host/owner/name.git, git@host:owner/name.git
// or ssh://git@host:port/owner/name.
func parseRemote(raw string) (host, slug string, err error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		u, perr := url.Parse(raw)
		if perr != nil || u.Hostname() == "" {
			return "", "", fmt.Errorf("cannot read the remote %q", raw)
		}
		return u.Hostname(), strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), nil
	}
	if m := scpRemote.FindStringSubmatch(raw); m != nil {
		return m[1], m[2], nil
	}
	return "", "", fmt.Errorf("cannot read the remote %q", raw)
}

// forgeFor picks the tool for a host: ORBIT_FORGE (gh | glab) decides, then github.com is GitHub and anything else
// is taken for a GitLab, which is what a company host usually is.
func forgeFor(host string) Forge {
	switch os.Getenv("ORBIT_FORGE") {
	case "gh":
		return github{}
	case "glab":
		return gitlab{}
	}
	if strings.Contains(host, "github") {
		return github{}
	}
	return gitlab{}
}

// repoInfo is what does not change while orbit runs: where the repo is and what it is.
type repoInfo struct {
	root, slug string
	forge      Forge
	err        error
}

// repos finds the repository of a directory, remembering what it learned.
type repos struct {
	mu    sync.Mutex
	known map[string]repoInfo
	git   func(dir string, args ...string) (string, error)
	pick  func(host string) Forge
}

func newRepos() *repos { return &repos{known: map[string]repoInfo{}, git: gitOutput, pick: forgeFor} }

func (r *repos) of(dir string) (*Repo, error) {
	if dir == "" {
		return nil, errNoRepo
	}
	r.mu.Lock()
	info, ok := r.known[dir]
	r.mu.Unlock()
	if !ok {
		info = r.discover(dir)
		r.mu.Lock()
		r.known[dir] = info
		r.mu.Unlock()
	}
	if info.err != nil {
		return nil, info.err
	}
	branch, _ := r.git(info.root, "branch", "--show-current")
	return &Repo{Root: info.root, Branch: branch, Slug: info.slug, Forge: info.forge}, nil
}

func (r *repos) discover(dir string) repoInfo {
	root, err := r.git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return repoInfo{err: errNoRepo}
	}
	root = filepath.Clean(root)
	remote := r.remoteOf(root)
	if remote == "" {
		return repoInfo{root: root, err: errors.New("the repository has no remote")}
	}
	url, err := r.git(root, "remote", "get-url", remote)
	if err != nil {
		return repoInfo{root: root, err: fmt.Errorf("the remote %s has no url", remote)}
	}
	host, slug, err := parseRemote(url)
	if err != nil {
		return repoInfo{root: root, err: err}
	}
	return repoInfo{root: root, slug: slug, forge: r.pick(host)}
}

// remoteOf is the remote the current branch tracks, else origin, else the first one.
func (r *repos) remoteOf(root string) string {
	if branch, _ := r.git(root, "branch", "--show-current"); branch != "" {
		if remote, err := r.git(root, "config", "branch."+branch+".remote"); err == nil && remote != "" && remote != "." {
			return remote
		}
	}
	list, _ := r.git(root, "remote")
	remotes := strings.Fields(list)
	for _, n := range remotes {
		if n == "origin" {
			return n
		}
	}
	if len(remotes) > 0 {
		return remotes[0]
	}
	return ""
}
