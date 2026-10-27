package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/JGoterris/orbit/pluginsdk"
	"github.com/JGoterris/orbit/pluginsdk/protocol"
)

const defaultInterval = 30 * time.Second

// notifier is what the plugin tells orbit on its own: the SDK does it, the tests record it.
type notifier interface {
	SetServiceBadges(service string, badges ...protocol.Badge)
	SetHeaderBadges(badges ...protocol.Badge)
	Invalidate(view, pane string)
	Toast(tone protocol.Tone, text string)
}

// app is the plugin: what the panes, the actions and the badges share.
type app struct {
	p     *pluginsdk.Plugin
	n     notifier
	repos *repos

	mu       sync.Mutex
	services []protocol.ServiceRef
	all      bool              // the CI list shows every branch
	urls     map[string]string // "<root>|mr|<id>" and "<root>|run|<id>" → the page, for "open in the browser"
	runs     map[string]Run    // "<root>|<id>" → the run, for "run again"
	// what the last poll saw, for the badges and the sections of the detail
	mrOf   map[string]MR // service → the merge request of its branch
	runOf  map[string]Run
	badges map[string]string // service → the badge sent, to send only what changed
	header string
	seen   map[string]string // "<root>|<branch>" → status of its latest run, to tell when one finishes

	open func(url string) error
	// afterAction is what follows an action that changed something: look again, so the badges say it
	afterAction func()

	actions map[string]actionFunc
}

type actionFunc = func(pluginsdk.ActionRequest) (*protocol.InvokeResult, error)

func (a *app) action(id string, fn actionFunc) {
	a.actions[id] = fn
	a.p.Action(id, fn)
}

func (a *app) act(id string, r pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
	fn := a.actions[id]
	if fn == nil {
		return nil, fmt.Errorf("no action %s", id)
	}
	r.Action = id
	return fn(r)
}

func newApp() *app {
	a := &app{
		actions: map[string]actionFunc{}, repos: newRepos(), urls: map[string]string{}, runs: map[string]Run{}, mrOf: map[string]MR{}, runOf: map[string]Run{},
		badges: map[string]string{}, seen: map[string]string{}, open: openBrowser,
	}
	a.afterAction = func() { go a.refresh() }
	return a
}

func (a *app) setServices(s []protocol.ServiceRef) {
	a.mu.Lock()
	a.services = s
	a.mu.Unlock()
}

func (a *app) cwdOf(service string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.services {
		if s.Name == service {
			return s.Cwd
		}
	}
	return ""
}

func (a *app) repoOf(service string) (*Repo, error) {
	if service == "" {
		return nil, errors.New("no service selected")
	}
	return a.repos.of(a.cwdOf(service))
}

func (a *app) remember(key, url string) {
	a.mu.Lock()
	a.urls[key] = url
	a.mu.Unlock()
}

func (a *app) urlOf(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.urls[key]
}

func serve() error {
	a := newApp()
	a.p = pluginsdk.New()
	a.n = a.p
	a.register()
	return a.p.Run()
}

var (
	mrColumns = []protocol.Column{
		{ID: "title"}, {ID: "checks", Priority: 1, Width: 6}, {ID: "review", Priority: 2, Width: 8},
		{ID: "author", Priority: 3, Width: 12}, {ID: "branch", Priority: 4, Width: 20}, {ID: "when", Priority: 5, Width: 4},
	}
	ciColumns = []protocol.Column{
		{ID: "title"}, {ID: "status", Priority: 1, Width: 9}, {ID: "event", Priority: 3, Width: 12},
		{ID: "branch", Priority: 2, Width: 20}, {ID: "when", Priority: 4, Width: 4},
	}
)

func (a *app) register() {
	p := a.p
	p.OnInit(func(i pluginsdk.Init) error {
		a.setServices(i.Services)
		interval := defaultInterval
		var cfg struct{ Interval string }
		if i.Settings(&cfg) == nil && cfg.Interval != "" {
			if d, err := time.ParseDuration(cfg.Interval); err == nil && d >= 5*time.Second {
				interval = d
			}
		}
		go a.poll(interval)
		return nil
	})
	p.OnEvent(func(ev protocol.Event) {
		if ev.Kind == protocol.EventConfig && ev.Services != nil {
			a.setServices(ev.Services)
			a.refresh()
		}
	})

	p.Pane("mr", "list", a.mrList)
	p.Pane("mr", "info", a.mrInfo)
	p.Pane("ci", "list", a.ciList)
	p.Pane("ci", "log", a.ciLog)
	p.Sections("mr", a.section)

	a.action("mr-open", func(r pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
		return a.openPage(r, "mr")
	})
	a.action("ci-open", func(r pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
		return a.openPage(r, "run")
	})
	a.action("mr-checkout", a.onMR("checked out the branch of", func(f Forge, repo *Repo, id string, _ map[string]string) error {
		return f.Checkout(repo, id)
	}))
	a.action("mr-approve", a.onMR("approved", func(f Forge, repo *Repo, id string, _ map[string]string) error {
		return f.Approve(repo, id)
	}))
	a.action("mr-merge", a.onMR("merged", func(f Forge, repo *Repo, id string, in map[string]string) error {
		return f.Merge(repo, id, in["method"])
	}))
	a.action("mr-close", a.onMR("closed", func(f Forge, repo *Repo, id string, _ map[string]string) error {
		return f.Close(repo, id)
	}))
	a.action("ci-rerun", a.onRun("started again", "ci", func(f Forge, repo *Repo, run Run) error {
		return f.Rerun(repo, run.ID, run.Status == "failure")
	}))
	a.action("ci-cancel", a.onRun("cancelled", "ci", func(f Forge, repo *Repo, run Run) error {
		return f.Cancel(repo, run.ID)
	}))
	a.action("ci-all", func(r pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
		a.mu.Lock()
		a.all = !a.all
		all := a.all
		a.mu.Unlock()
		if all {
			return pluginsdk.Done(protocol.ToneInfo, "runs of all the branches", "ci"), nil
		}
		return pluginsdk.Done(protocol.ToneInfo, "runs of the branch of the service", "ci"), nil
	})
}

// noRepo turns "this service is not in a repository" into an empty list that says so, and any other trouble into an
// error the pane shows.
func noRepo(service string, err error) (protocol.ViewData, error) {
	if errors.Is(err, errNoRepo) {
		return pluginsdk.EmptyList(service + " is not in a git repository"), nil
	}
	return protocol.ViewData{}, err
}

func tone(m map[string]protocol.Tone, key string) protocol.Tone { return m[key] }

var (
	checkTones  = map[string]protocol.Tone{"pass": protocol.ToneOK, "fail": protocol.ToneError, "run": protocol.ToneWarn, "wait": protocol.ToneMuted, "skip": protocol.ToneMuted}
	reviewTones = map[string]protocol.Tone{"approved": protocol.ToneOK, "changes": protocol.ToneError, "review": protocol.ToneMuted}
	stateTones  = map[string]protocol.Tone{"open": protocol.ToneOK, "draft": protocol.ToneMuted, "merged": protocol.ToneAccent, "closed": protocol.ToneMuted}
	stateIcons  = map[string]string{"open": "open", "draft": "draft", "merged": "merge", "closed": "closed"}
	runTones    = map[string]protocol.Tone{"success": protocol.ToneOK, "failure": protocol.ToneError, "running": protocol.ToneWarn, "queued": protocol.ToneMuted, "cancelled": protocol.ToneMuted, "skipped": protocol.ToneMuted}
	runIcons    = map[string]string{"success": "ok", "failure": "fail", "running": "run", "queued": "wait", "cancelled": "skip", "skipped": "skip"}
	jobGlyphs   = map[string]string{"success": "✓", "failure": "✖", "running": "◐", "queued": "◌", "cancelled": "⊘", "skipped": "⊘"}
	checkText   = map[string]string{"pass": "pass", "fail": "fail", "run": "running", "wait": "waiting", "skip": "skipped"}
)

func (a *app) mrList(r pluginsdk.Request) (protocol.ViewData, error) {
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return noRepo(r.Context.Service, err)
	}
	mrs, err := repo.Forge.ListMRs(repo)
	if err != nil {
		return protocol.ViewData{}, err
	}
	prefix := repo.Forge.Prefix()
	items := make([]protocol.Item, 0, len(mrs))
	for _, m := range mrs {
		a.remember(repo.Root+"|mr|"+m.ID, m.URL)
		items = append(items, protocol.Item{ID: m.ID, Icon: stateIcons[m.State], Cells: map[string]protocol.Cell{
			"title":  pluginsdk.Cell(prefix + m.ID + " " + m.Title),
			"checks": pluginsdk.Cell(checkText[m.Checks], tone(checkTones, m.Checks)),
			"review": pluginsdk.Cell(m.Review, tone(reviewTones, m.Review)),
			"author": pluginsdk.Cell(m.Author),
			"branch": pluginsdk.Cell(m.Branch, protocol.ToneMuted),
			"when":   pluginsdk.Cell(ago(m.Updated), protocol.ToneMuted),
		}})
	}
	d := pluginsdk.ListData(mrColumns, items...)
	d.List.Empty = "no open " + map[string]string{"github": "pull", "gitlab": "merge"}[repo.Forge.Kind()] + " requests in " + repo.Slug
	d.List.Footer = repo.Slug + " · " + repo.Forge.Kind()
	return d, nil
}

func (a *app) mrInfo(r pluginsdk.Request) (protocol.ViewData, error) {
	id := r.Context.Selection["list"]
	if id == "" {
		return pluginsdk.DetailData(), nil
	}
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return protocol.ViewData{}, err
	}
	m, err := repo.Forge.ViewMR(repo, id)
	if err != nil {
		return protocol.ViewData{}, err
	}
	head := protocol.Section{
		Title: repo.Forge.Prefix() + m.ID + " " + m.Title,
		Fields: []protocol.Field{
			pluginsdk.Field("state", m.State, tone(stateTones, m.State)),
			pluginsdk.Field("author", m.Author),
			pluginsdk.Field("branch", m.Branch+" → "+m.Target),
		},
	}
	if m.Review != "" {
		head.Fields = append(head.Fields, pluginsdk.Field("review", m.Review, tone(reviewTones, m.Review)))
	}
	if m.Checks != "" {
		head.Fields = append(head.Fields, pluginsdk.Field("checks", checkText[m.Checks], tone(checkTones, m.Checks)))
	}
	if m.Mergeable != "" {
		t := protocol.ToneWarn
		if m.Mergeable == "ready" {
			t = protocol.ToneOK
		}
		head.Fields = append(head.Fields, pluginsdk.Field("merge", m.Mergeable, t))
	}
	if ago(m.Updated) != "" {
		head.Fields = append(head.Fields, pluginsdk.Field("updated", ago(m.Updated)+" ago"))
	}
	for i, line := range strings.Split(strings.TrimSpace(plain(m.Body)), "\n") {
		if i >= 12 {
			head.Text = append(head.Text, pluginsdk.Line(pluginsdk.Span("…", protocol.ToneMuted)))
			break
		}
		head.Text = append(head.Text, pluginsdk.Line(pluginsdk.Span(strings.TrimRight(line, " "), protocol.ToneMuted)))
	}
	sections := []protocol.Section{head}
	if len(m.CheckList) > 0 {
		checks := protocol.Section{Title: "Checks"}
		for _, c := range m.CheckList {
			checks.Fields = append(checks.Fields, pluginsdk.Field(c.Name, checkText[c.Status], tone(checkTones, c.Status)))
		}
		sections = append(sections, checks)
	}
	return pluginsdk.DetailData(sections...), nil
}

func (a *app) ciList(r pluginsdk.Request) (protocol.ViewData, error) {
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return noRepo(r.Context.Service, err)
	}
	a.mu.Lock()
	all := a.all
	a.mu.Unlock()
	branch := repo.Branch
	if all {
		branch = ""
	}
	runs, err := repo.Forge.ListRuns(repo, branch)
	if err != nil {
		return protocol.ViewData{}, err
	}
	items := make([]protocol.Item, 0, len(runs))
	for _, x := range runs {
		a.remember(repo.Root+"|run|"+x.ID, x.URL)
		a.mu.Lock()
		a.runs[repo.Root+"|"+x.ID] = x
		a.mu.Unlock()
		items = append(items, protocol.Item{ID: x.ID, Icon: runIcons[x.Status], Cells: map[string]protocol.Cell{
			"title":  pluginsdk.Cell(x.Name + " · " + x.Title),
			"status": pluginsdk.Cell(x.Status, tone(runTones, x.Status)),
			"event":  pluginsdk.Cell(x.Event, protocol.ToneMuted),
			"branch": pluginsdk.Cell(x.Branch, protocol.ToneMuted),
			"when":   pluginsdk.Cell(ago(x.Created), protocol.ToneMuted),
		}})
	}
	d := pluginsdk.ListData(ciColumns, items...)
	where := "branch " + repo.Branch + " · b: all the branches"
	if all {
		where = "all the branches · b: only " + repo.Branch
	}
	if repo.Branch == "" {
		where = "all the branches"
	}
	d.List.Footer = repo.Slug + " · " + where
	d.List.Empty = "no runs"
	return d, nil
}

func (a *app) ciLog(r pluginsdk.Request) (protocol.ViewData, error) {
	id := r.Context.Selection["list"]
	if id == "" {
		return pluginsdk.TextData(), nil
	}
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return protocol.ViewData{}, err
	}
	jobs, err := repo.Forge.Jobs(repo, id)
	if err != nil {
		return protocol.ViewData{}, err
	}
	var lines [][]protocol.Span
	failed := false
	for _, j := range jobs {
		lines = append(lines, pluginsdk.Line(
			pluginsdk.Span(jobGlyphs[j.Status]+" ", tone(runTones, j.Status)), pluginsdk.Span(j.Name), pluginsdk.Span("  "+j.Status, protocol.ToneMuted)))
		failed = failed || j.Status == "failure"
	}
	if len(jobs) == 0 {
		lines = append(lines, pluginsdk.Line(pluginsdk.Span("no jobs yet", protocol.ToneMuted)))
	}
	if failed {
		out, err := repo.Forge.Log(repo, id, true)
		if err != nil {
			return protocol.ViewData{}, err
		}
		lines = append(lines, nil, pluginsdk.Line(pluginsdk.Span("failed jobs", protocol.ToneError)))
		for _, l := range tail(out, 150) {
			t := protocol.Tone("")
			if strings.Contains(strings.ToLower(l), "error") || strings.Contains(strings.ToLower(l), "fail") {
				t = protocol.ToneError
			}
			lines = append(lines, pluginsdk.Line(pluginsdk.Span(l, t)))
		}
	}
	return pluginsdk.TextData(lines...), nil
}

// section is the block of the detail of a service: the merge request of its branch, as the last poll saw it.
func (a *app) section(service string) ([]protocol.Section, error) {
	a.mu.Lock()
	m, ok := a.mrOf[service]
	a.mu.Unlock()
	if !ok {
		return nil, nil
	}
	repo, err := a.repoOf(service)
	if err != nil {
		return nil, nil
	}
	return []protocol.Section{{Fields: []protocol.Field{
		pluginsdk.Field(repo.Forge.Prefix()+m.ID, m.Title),
		pluginsdk.Field("state", m.State, tone(stateTones, m.State)),
		pluginsdk.Field("checks", checkText[m.Checks], tone(checkTones, m.Checks)),
		pluginsdk.Field("review", m.Review, tone(reviewTones, m.Review)),
	}}}, nil
}

func (a *app) onMR(done string, do func(f Forge, repo *Repo, id string, inputs map[string]string) error) func(pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
	return func(r pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
		id := r.Context.Selection["list"]
		if id == "" {
			return nil, errors.New("select a merge request first")
		}
		repo, err := a.repoOf(r.Context.Service)
		if err != nil {
			return nil, err
		}
		if err := do(repo.Forge, repo, id, r.Inputs); err != nil {
			return nil, err
		}
		a.afterAction()
		return pluginsdk.Done(protocol.ToneOK, fmt.Sprintf("%s %s%s", done, repo.Forge.Prefix(), id), "mr"), nil
	}
}

func (a *app) onRun(done, view string, do func(f Forge, repo *Repo, run Run) error) func(pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
	return func(r pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
		id := r.Context.Selection["list"]
		if id == "" {
			return nil, errors.New("select a run first")
		}
		repo, err := a.repoOf(r.Context.Service)
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		run, ok := a.runs[repo.Root+"|"+id]
		a.mu.Unlock()
		if !ok {
			run = Run{ID: id}
		}
		if err := do(repo.Forge, repo, run); err != nil {
			return nil, err
		}
		a.afterAction()
		return pluginsdk.Done(protocol.ToneOK, fmt.Sprintf("run %s %s", id, done), view), nil
	}
}

func (a *app) openPage(r pluginsdk.ActionRequest, kind string) (*protocol.InvokeResult, error) {
	id := r.Context.Selection["list"]
	if id == "" {
		return nil, errors.New("select something first")
	}
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return nil, err
	}
	url := a.urlOf(repo.Root + "|" + kind + "|" + id)
	if url == "" {
		return nil, errors.New("the page of this one is not known yet: reload the list")
	}
	if err := a.open(url); err != nil {
		return nil, err
	}
	return pluginsdk.Done(protocol.ToneInfo, "opened "+url, "none"), nil
}

func (a *app) poll(interval time.Duration) {
	a.refresh()
	for range time.Tick(interval) {
		a.refresh()
	}
}

type polled struct {
	mr      *MR
	run     *Run
	hasRun  bool
	service string
	repo    *Repo
}

// refresh asks each repository once (not once per service), then puts a badge on every service, says in the header
// how CI is doing and tells when a run finished.
func (a *app) refresh() {
	a.mu.Lock()
	services := append([]protocol.ServiceRef(nil), a.services...)
	a.mu.Unlock()

	type repoData struct {
		repo *Repo
		mrs  map[string]MR // by branch
		runs map[string]Run
		err  error
	}
	byRoot := map[string]*repoData{}
	var results []polled
	for _, s := range services {
		repo, err := a.repos.of(s.Cwd)
		if err != nil {
			results = append(results, polled{service: s.Name})
			continue
		}
		rd := byRoot[repo.Root]
		if rd == nil {
			rd = &repoData{repo: repo, mrs: map[string]MR{}, runs: map[string]Run{}}
			byRoot[repo.Root] = rd
			if mrs, err := repo.Forge.ListMRs(repo); err == nil {
				for _, m := range mrs {
					rd.mrs[m.Branch] = m
				}
			} else {
				rd.err = err
			}
		}
		res := polled{service: s.Name, repo: repo}
		if m, ok := rd.mrs[repo.Branch]; ok && repo.Branch != "" {
			mr := m
			res.mr = &mr
		}
		if repo.Branch != "" {
			key := repo.Root + "|" + repo.Branch
			run, ok := rd.runs[key]
			if !ok {
				if runs, err := repo.Forge.ListRuns(repo, repo.Branch); err == nil && len(runs) > 0 {
					run, ok = runs[0], true
					rd.runs[key] = run
				}
			}
			if ok {
				r := run
				res.run, res.hasRun = &r, true
			}
		}
		results = append(results, res)
	}
	a.apply(results)
}

func (a *app) apply(results []polled) {
	changed := false
	fails, running, known := 0, 0, 0
	for _, r := range results {
		var badges []protocol.Badge
		switch {
		case r.mr != nil:
			icon := stateIcons[r.mr.State]
			t := protocol.ToneMuted
			switch r.mr.Checks {
			case "pass":
				icon, t = "ok", protocol.ToneOK
			case "fail":
				icon, t = "fail", protocol.ToneError
			case "run":
				icon, t = "run", protocol.ToneWarn
			}
			badges = []protocol.Badge{{Text: r.repo.Forge.Prefix() + r.mr.ID, Tone: t, Icon: icon}}
		case r.run != nil:
			badges = []protocol.Badge{{Text: "CI", Tone: runTones[r.run.Status], Icon: runIcons[r.run.Status]}}
		}
		if r.run != nil {
			known++
			switch r.run.Status {
			case "failure":
				fails++
			case "running", "queued":
				running++
			}
			a.finished(r)
		}
		a.mu.Lock()
		if r.mr != nil {
			a.mrOf[r.service] = *r.mr
		} else {
			delete(a.mrOf, r.service)
		}
		if r.run != nil {
			a.runOf[r.service] = *r.run
		}
		sig, _ := json.Marshal(badges)
		if a.badges[r.service] != string(sig) {
			a.badges[r.service] = string(sig)
			changed = true
			a.mu.Unlock()
			a.n.SetServiceBadges(r.service, badges...)
		} else {
			a.mu.Unlock()
		}
	}
	header := protocol.Badge{}
	switch {
	case fails > 0:
		header = protocol.Badge{Text: fmt.Sprintf("CI %d", fails), Tone: protocol.ToneError, Icon: "fail"}
	case running > 0:
		header = protocol.Badge{Text: "CI", Tone: protocol.ToneWarn, Icon: "run"}
	case known > 0:
		header = protocol.Badge{Text: "CI", Tone: protocol.ToneOK, Icon: "ok"}
	}
	sig, _ := json.Marshal(header)
	a.mu.Lock()
	headerChanged := a.header != string(sig)
	a.header = string(sig)
	a.mu.Unlock()
	if headerChanged {
		if header.Text == "" {
			a.n.SetHeaderBadges()
		} else {
			a.n.SetHeaderBadges(header)
		}
		changed = true
	}
	if changed {
		a.n.Invalidate("", "")
	}
}

func (a *app) finished(r polled) {
	key := r.repo.Root + "|" + r.repo.Branch
	a.mu.Lock()
	prev := a.seen[key]
	a.seen[key] = r.run.Status
	a.mu.Unlock()
	if prev != "running" && prev != "queued" {
		return
	}
	switch r.run.Status {
	case "success":
		a.n.Toast(protocol.ToneOK, fmt.Sprintf("CI passed on %s (%s)", r.service, r.repo.Branch))
	case "failure":
		a.n.Toast(protocol.ToneError, fmt.Sprintf("CI failed on %s (%s)", r.service, r.repo.Branch))
	}
}
