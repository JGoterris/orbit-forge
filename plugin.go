package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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

	sched *schedules
	// jobs are the jobs of the runs lately asked for, which the graph and the log both need: "<root>|<id>"
	jobs map[string]cachedJobs

	open func(url string) error
	now  func() time.Time
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
		badges: map[string]string{}, seen: map[string]string{}, open: openBrowser, jobs: map[string]cachedJobs{}, now: time.Now,
		sched: newSchedules(""),
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
		{ID: "title"}, {ID: "checks", Priority: 1, Width: 7}, {ID: "review", Priority: 2, Width: 8},
		{ID: "size", Priority: 4, Width: 11}, {ID: "author", Priority: 3, Width: 12}, {ID: "branch", Priority: 5, Width: 20},
		{ID: "when", Priority: 6, Width: 4},
	}
	// the state of a run is its icon: the columns leave the room to its title
	ciColumns = []protocol.Column{
		{ID: "title"}, {ID: "branch", Priority: 1, Width: 16}, {ID: "event", Priority: 3, Width: 12}, {ID: "when", Priority: 2, Width: 4},
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
		a.sched = newSchedules(i.StateDir)
		go a.poll(interval)
		go a.runSchedules()
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
	p.Pane("ci", "graph", a.ciGraph)
	p.Pane("ci", "log", a.ciLog)
	p.Options("ci-run", "workflow", a.workflowOptions)
	p.Options("ci-run", "branch", a.branchOptions)
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
	a.action("mr-comment", a.onMR("commented on", func(f Forge, repo *Repo, id string, in map[string]string) error {
		body := strings.TrimSpace(in["body"])
		if body == "" {
			return errors.New("an empty comment says nothing")
		}
		return f.Comment(repo, id, body)
	}))
	a.action("mr-draft", func(r pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
		id := r.Context.Selection["list"]
		if id == "" {
			return nil, errors.New("select a merge request first")
		}
		repo, err := a.repoOf(r.Context.Service)
		if err != nil {
			return nil, err
		}
		m, err := repo.Forge.ViewMR(repo, id)
		if err != nil {
			return nil, err
		}
		draft := m.State != "draft"
		if err := repo.Forge.SetDraft(repo, id, draft); err != nil {
			return nil, err
		}
		a.afterAction()
		if draft {
			return pluginsdk.Done(protocol.ToneOK, fmt.Sprintf("%s%s is a draft now", repo.Forge.Prefix(), id), "mr"), nil
		}
		return pluginsdk.Done(protocol.ToneOK, fmt.Sprintf("%s%s is ready for review", repo.Forge.Prefix(), id), "mr"), nil
	})
	a.action("ci-rerun", a.onRun("started again", "ci", func(f Forge, repo *Repo, run Run) error {
		return f.Rerun(repo, run.ID, run.Status == "failure")
	}))
	a.action("ci-cancel", func(r pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
		if id := r.Context.Selection["list"]; isScheduled(id) {
			x, ok, err := a.sched.remove(id)
			switch {
			case err != nil:
				return nil, err
			case !ok:
				return nil, errors.New("it is not scheduled any more")
			}
			return pluginsdk.Done(protocol.ToneOK, fmt.Sprintf("%s on %s will not run", x.Name, x.Branch), "ci"), nil
		}
		return a.onRun("cancelled", "ci", func(f Forge, repo *Repo, run Run) error {
			return f.Cancel(repo, run.ID)
		})(r)
	})
	a.action("ci-run", a.startRun)
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
	checkGlyphs = map[string]string{"pass": "✓", "fail": "✖", "run": "◐", "wait": "◌", "skip": "⊘"}
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
			"size":   pluginsdk.Cell(size(m), protocol.ToneMuted),
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
	return pluginsdk.DetailData(mrSections(repo.Forge.Prefix(), m)...), nil
}

// size is how big a merge request is: "+120 −30", or the files it changes when that is all that is known.
func size(m MR) string {
	switch {
	case m.Additions+m.Deletions > 0:
		return fmt.Sprintf("+%d −%d", m.Additions, m.Deletions)
	case m.Files > 0:
		return fmt.Sprintf("%d files", m.Files)
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

var reviewerTones = map[string]protocol.Tone{"approved": protocol.ToneOK, "changes": protocol.ToneError, "commented": protocol.ToneInfo, "pending": protocol.ToneMuted}

const (
	maxFiles = 100
	maxNotes = 30
)

// mrSections are the details of a merge request: what it is, who reviews it, its checks, its description, the
// files it changes, its commits and its conversation.
func mrSections(prefix string, m MR) []protocol.Section {
	head := protocol.Section{
		Title: prefix + m.ID + " " + m.Title,
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
	if m.AutoMerge {
		head.Fields = append(head.Fields, pluginsdk.Field("auto-merge", "on", protocol.ToneInfo))
	}
	var changes []string
	if sz := size(m); sz != "" && m.Additions+m.Deletions > 0 {
		changes = append(changes, sz)
	}
	if n := max(m.Files, len(m.FileList)); n > 0 {
		changes = append(changes, plural(n, "file", "files"))
	}
	if n := len(m.CommitList); n > 0 {
		changes = append(changes, plural(n, "commit", "commits"))
	}
	if n := max(m.Comments, len(m.Notes)); n > 0 {
		changes = append(changes, plural(n, "comment", "comments"))
	}
	if len(changes) > 0 {
		head.Fields = append(head.Fields, pluginsdk.Field("changes", strings.Join(changes, " · ")))
	}
	if ago(m.Created) != "" {
		head.Fields = append(head.Fields, pluginsdk.Field("opened", ago(m.Created)+" ago"))
	}
	if ago(m.Updated) != "" {
		head.Fields = append(head.Fields, pluginsdk.Field("updated", ago(m.Updated)+" ago"))
	}
	if len(m.Assignees) > 0 {
		head.Fields = append(head.Fields, pluginsdk.Field("assignees", strings.Join(m.Assignees, ", ")))
	}
	if m.Milestone != "" {
		head.Fields = append(head.Fields, pluginsdk.Field("milestone", m.Milestone))
	}
	for _, l := range m.Labels {
		head.Badges = append(head.Badges, protocol.Badge{Text: l, Tone: protocol.ToneInfo})
	}
	sections := []protocol.Section{head}

	if len(m.Reviewers) > 0 {
		rev := protocol.Section{Title: "Reviewers"}
		for _, r := range m.Reviewers {
			rev.Fields = append(rev.Fields, pluginsdk.Field(r.Name, r.State, tone(reviewerTones, r.State)))
		}
		sections = append(sections, rev)
	}
	if len(m.CheckList) > 0 {
		counts := map[string]int{}
		for _, c := range m.CheckList {
			counts[c.Status]++
		}
		title := "Checks"
		for _, st := range []string{"pass", "fail", "run", "wait", "skip"} {
			if counts[st] > 0 {
				title += fmt.Sprintf(" · %d %s", counts[st], checkText[st])
			}
		}
		checks := protocol.Section{Title: title}
		// the ones that need looking at first
		rank := map[string]int{"fail": 0, "run": 1, "wait": 2, "pass": 3, "skip": 4}
		list := slices.Clone(m.CheckList)
		slices.SortStableFunc(list, func(x, y Check) int { return rank[x.Status] - rank[y.Status] })
		for _, c := range list {
			line := pluginsdk.Line(pluginsdk.Span(checkGlyphs[c.Status]+" ", tone(checkTones, c.Status)), pluginsdk.Span(c.Name))
			if c.Status != "pass" {
				// a ✓ says it passed; the rest say what they are
				line = append(line, pluginsdk.Span("  "+checkText[c.Status], tone(checkTones, c.Status)))
			}
			checks.Text = append(checks.Text, line)
		}
		sections = append(sections, checks)
	}
	if body := markdown(m.Body, ""); len(body) > 0 {
		sections = append(sections, protocol.Section{Title: "Description", Text: body})
	} else {
		sections = append(sections, protocol.Section{Title: "Description", Text: [][]protocol.Span{pluginsdk.Line(pluginsdk.Span("no description", protocol.ToneMuted))}})
	}
	if len(m.FileList) > 0 {
		files := protocol.Section{Title: "Files · " + plural(len(m.FileList), "file", "files")}
		addW, delW := 0, 0
		for _, f := range m.FileList {
			addW, delW = max(addW, len(fmt.Sprint(f.Additions))), max(delW, len(fmt.Sprint(f.Deletions)))
		}
		for i, f := range m.FileList {
			if i == maxFiles {
				files.Text = append(files.Text, pluginsdk.Line(pluginsdk.Span(fmt.Sprintf("… and %d more", len(m.FileList)-maxFiles), protocol.ToneMuted)))
				break
			}
			files.Text = append(files.Text, pluginsdk.Line(
				pluginsdk.Span(fmt.Sprintf("+%-*d ", addW, f.Additions), protocol.ToneOK),
				pluginsdk.Span(fmt.Sprintf("−%-*d  ", delW, f.Deletions), protocol.ToneError),
				pluginsdk.Span(f.Path)))
		}
		sections = append(sections, files)
	}
	if len(m.CommitList) > 0 {
		commits := protocol.Section{Title: "Commits · " + plural(len(m.CommitList), "commit", "commits")}
		for _, c := range m.CommitList {
			line := pluginsdk.Line(pluginsdk.Span(shortSHA(c.SHA)+" ", protocol.ToneAccent), pluginsdk.Span(c.Title))
			if meta := strings.Trim(c.Author+" · "+ago(c.When), " ·"); meta != "" {
				line = append(line, pluginsdk.Span("  "+meta, protocol.ToneMuted))
			}
			commits.Text = append(commits.Text, line)
		}
		sections = append(sections, commits)
	}
	if len(m.Notes) > 0 {
		notes := m.Notes
		title := "Conversation · " + plural(len(notes), "comment", "comments")
		if len(notes) > maxNotes {
			notes = notes[len(notes)-maxNotes:]
			title += fmt.Sprintf(" · the last %d", maxNotes)
		}
		conv := protocol.Section{Title: title}
		for i, n := range notes {
			if i > 0 {
				conv.Text = append(conv.Text, nil)
			}
			head := pluginsdk.Line(protocol.Span{Text: n.Author, Tone: protocol.ToneAccent, Bold: true})
			if w := ago(n.When); w != "" {
				head = append(head, pluginsdk.Span(" · "+w+" ago", protocol.ToneMuted))
			}
			conv.Text = append(conv.Text, head)
			for _, l := range markdown(n.Body, "") {
				conv.Text = append(conv.Text, append([]protocol.Span{pluginsdk.Span("  ")}, l...))
			}
		}
		sections = append(sections, conv)
	}
	return sections
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
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
	var items []protocol.Item
	now := a.now()
	for _, x := range a.sched.of(repo.Root) {
		items = append(items, protocol.Item{ID: x.ID, Icon: "wait", Cells: map[string]protocol.Cell{
			"title":  pluginsdk.Cell("⏲ "+x.Name+" · "+until(x.At, now), protocol.ToneInfo),
			"event":  pluginsdk.Cell("scheduled", protocol.ToneInfo),
			"branch": pluginsdk.Cell(x.Branch, protocol.ToneMuted),
		}})
	}
	for _, x := range runs {
		a.remember(repo.Root+"|run|"+x.ID, x.URL)
		a.mu.Lock()
		a.runs[repo.Root+"|"+x.ID] = x
		a.mu.Unlock()
		items = append(items, protocol.Item{ID: x.ID, Icon: runIcons[x.Status], Cells: map[string]protocol.Cell{
			"title":  pluginsdk.Cell(x.Name + " · " + runTitle(x.Title)),
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
	d.List.Empty = "no runs · s: run one"
	return d, nil
}

var mergeTitle = regexp.MustCompile(`^Merge pull request #(\d+) from [^/\s]+/(\S+)`)

// runTitle is the title of a run as a list wants it: "Merge pull request #10 from owner/feature/x" says only which
// pull request, so it is "#10 feature/x".
func runTitle(title string) string {
	if m := mergeTitle.FindStringSubmatch(title); m != nil {
		return "#" + m[1] + " " + m[2]
	}
	return title
}

func isScheduled(id string) bool { return strings.HasPrefix(id, "at-") }

// cachedJobs are the jobs of a run as the forge said them a moment ago.
type cachedJobs struct {
	jobs []Job
	at   time.Time
}

// jobsTTL is how long the jobs of a run are taken as they are: the graph and the log ask for them together.
const jobsTTL = 5 * time.Second

func (a *app) jobsOf(repo *Repo, id string) ([]Job, error) {
	key := repo.Root + "|" + id
	a.mu.Lock()
	c, ok := a.jobs[key]
	a.mu.Unlock()
	if ok && time.Since(c.at) < jobsTTL {
		return c.jobs, nil
	}
	jobs, err := repo.Forge.Jobs(repo, id)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	for k, v := range a.jobs {
		if time.Since(v.at) > jobsTTL {
			delete(a.jobs, k)
		}
	}
	a.jobs[key] = cachedJobs{jobs: jobs, at: time.Now()}
	a.mu.Unlock()
	return jobs, nil
}

// scheduledText is what the panes of a run say about one that is only scheduled.
func (a *app) scheduledText(root, id string) protocol.ViewData {
	for _, x := range a.sched.of(root) {
		if x.ID == id {
			return pluginsdk.TextData(
				pluginsdk.Line(protocol.Span{Text: "⏲ " + x.Name, Bold: true}),
				pluginsdk.Line(pluginsdk.Span("branch ", protocol.ToneMuted), pluginsdk.Span(x.Branch)),
				pluginsdk.Line(pluginsdk.Span("runs   ", protocol.ToneMuted), pluginsdk.Span(x.At.Format("Mon 2 Jan 15:04")+" ("+until(x.At, a.now())+")", protocol.ToneInfo)),
				nil,
				pluginsdk.Line(pluginsdk.Span("orbit starts it then, if it is open · C cancels it", protocol.ToneMuted)),
			)
		}
	}
	return pluginsdk.TextData(pluginsdk.Line(pluginsdk.Span("it is not scheduled any more", protocol.ToneMuted)))
}

// ciGraph is the run as GitHub and GitLab draw it: what it is, then its jobs joined to the ones they need.
func (a *app) ciGraph(r pluginsdk.Request) (protocol.ViewData, error) {
	id := r.Context.Selection["list"]
	if id == "" {
		return pluginsdk.TextData(), nil
	}
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return protocol.ViewData{}, err
	}
	if isScheduled(id) {
		return a.scheduledText(repo.Root, id), nil
	}
	jobs, err := a.jobsOf(repo, id)
	if err != nil {
		return protocol.ViewData{}, err
	}
	a.mu.Lock()
	run, known := a.runs[repo.Root+"|"+id]
	a.mu.Unlock()
	var lines [][]protocol.Span
	if known {
		lines = append(lines, pluginsdk.Line(
			pluginsdk.Span(jobGlyphs[run.Status]+" ", tone(runTones, run.Status)),
			protocol.Span{Text: strings.TrimSpace(run.Name + " · " + run.Title), Bold: true}))
		meta := []string{run.Branch}
		if run.SHA != "" {
			meta = append(meta, shortSHA(run.SHA))
		}
		meta = append(meta, run.Event)
		if w := ago(run.Created); w != "" {
			meta = append(meta, w+" ago")
		}
		lines = append(lines, pluginsdk.Line(pluginsdk.Span(strings.Join(slices.DeleteFunc(meta, func(s string) bool { return s == "" }), " · "), protocol.ToneMuted)))
	}
	if len(jobs) == 0 {
		return pluginsdk.TextData(append(lines, nil, pluginsdk.Line(pluginsdk.Span("no jobs yet", protocol.ToneMuted)))...), nil
	}
	lines = append(lines, summary(jobs), nil)
	lines = append(lines, graphLines(jobs, r.Context.Width)...)
	return pluginsdk.TextData(lines...), nil
}

// summary is one line for the jobs of a run: how many of each, and how long it took from the first to the last.
func summary(jobs []Job) []protocol.Span {
	counts := map[string]int{}
	var from, to time.Time
	running := false
	for _, j := range jobs {
		counts[j.Status]++
		if !j.Started.IsZero() && (from.IsZero() || j.Started.Before(from)) {
			from = j.Started
		}
		if j.Finished.After(to) {
			to = j.Finished
		}
		running = running || j.Status == "running"
	}
	var line []protocol.Span
	for _, st := range []string{"success", "failure", "running", "queued", "cancelled", "skipped"} {
		if counts[st] > 0 {
			line = append(line, pluginsdk.Span(fmt.Sprintf("%s %d  ", jobGlyphs[st], counts[st]), tone(runTones, st)))
		}
	}
	if running {
		to = time.Now()
	}
	if !from.IsZero() && to.After(from) {
		line = append(line, pluginsdk.Span("⏱ "+shortDuration(to.Sub(from)), protocol.ToneMuted))
	}
	return line
}

// ciLog is what to look at in a run: the steps of the jobs that failed or are running, and the end of the output
// of the failed ones.
func (a *app) ciLog(r pluginsdk.Request) (protocol.ViewData, error) {
	id := r.Context.Selection["list"]
	if id == "" {
		return pluginsdk.TextData(), nil
	}
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return protocol.ViewData{}, err
	}
	if isScheduled(id) {
		return pluginsdk.TextData(pluginsdk.Line(pluginsdk.Span("not run yet", protocol.ToneMuted))), nil
	}
	jobs, err := a.jobsOf(repo, id)
	if err != nil {
		return protocol.ViewData{}, err
	}
	var lines [][]protocol.Span
	failed := false
	for _, j := range jobs {
		if j.Status != "failure" && j.Status != "running" {
			continue
		}
		failed = failed || j.Status == "failure"
		if len(lines) > 0 {
			lines = append(lines, nil)
		}
		head := pluginsdk.Line(pluginsdk.Span(jobGlyphs[j.Status]+" ", tone(runTones, j.Status)), protocol.Span{Text: j.Name, Bold: true})
		if d := shortDuration(j.Duration()); d != "" {
			head = append(head, pluginsdk.Span("  "+d, protocol.ToneMuted))
		}
		lines = append(lines, head)
		// the steps that went well are only counted: it is the one that failed that matters
		passed := 0
		for _, st := range j.Steps {
			switch st.Status {
			case "skipped":
			case "success":
				passed++
			default:
				lines = append(lines, pluginsdk.Line(pluginsdk.Span("  "+jobGlyphs[st.Status]+" ", tone(runTones, st.Status)), pluginsdk.Span(st.Name, stepTone(st.Status))))
			}
		}
		if passed > 0 {
			lines = append(lines, pluginsdk.Line(pluginsdk.Span(fmt.Sprintf("  ✓ %s passed", plural(passed, "step", "steps")), protocol.ToneMuted)))
		}
	}
	if len(lines) == 0 {
		msg := "every job passed"
		switch {
		case len(jobs) == 0:
			msg = "no jobs yet"
		case slices.ContainsFunc(jobs, func(j Job) bool { return j.Status == "queued" }):
			msg = "waiting for the jobs to start"
		}
		lines = append(lines, pluginsdk.Line(pluginsdk.Span(msg, protocol.ToneMuted)))
	}
	if failed {
		out, err := repo.Forge.Log(repo, id, true)
		if err != nil {
			return protocol.ViewData{}, err
		}
		lines = append(lines, failureLines(out)...)
	}
	return pluginsdk.TextData(lines...), nil
}

// errorLine tells the lines that say what went wrong: the errors of the runner and of the usual tools.
var errorLine = regexp.MustCompile(`^(##\[error\]|--- FAIL|FAIL\s|panic:|Error:|ERROR|error(\[\w+\])?:|E\s{3}|npm ERR!|\S+\.\w+:\d+(:\d+)?: )`)

const (
	maxErrors     = 15
	maxOutputTail = 200
)

// failureLines are the output of the failed jobs made to be read: the errors first, then the output with the
// blocks the runner folds (setting up, caches, environments) folded to their title, and the end of it.
func failureLines(out string) [][]protocol.Span {
	var errs, body [][]protocol.Span
	inGroup := false
	// where the steps start in body, and the first error: the output starts at the step that failed
	var steps []int
	firstErr := -1
	for _, l := range strings.Split(plain(out), "\n") {
		trimmed := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(trimmed, "##[group]"):
			inGroup = true
			steps = append(steps, len(body))
			body = append(body, pluginsdk.Line(pluginsdk.Span("▸ "+strings.TrimPrefix(trimmed, "##[group]"), protocol.ToneMuted)))
			continue
		case strings.HasPrefix(trimmed, "##[endgroup]"):
			inGroup = false
			continue
		case strings.HasPrefix(trimmed, "section_start:"), strings.HasPrefix(trimmed, "section_end:"), strings.HasPrefix(trimmed, "##[debug]"):
			continue // the marks of GitLab's sections, and debug noise
		case strings.HasPrefix(l, "── "):
			inGroup = false
			steps = append(steps, len(body))
			body = append(body, nil, pluginsdk.Line(protocol.Span{Text: l, Tone: protocol.ToneAccent, Bold: true}))
			continue
		case inGroup:
			continue
		}
		text := strings.TrimPrefix(strings.TrimPrefix(trimmed, "##[error]"), "##[warning]")
		switch {
		case errorLine.MatchString(trimmed):
			if len(errs) < maxErrors {
				errs = append(errs, pluginsdk.Line(pluginsdk.Span("✖ ", protocol.ToneError), pluginsdk.Span(text, protocol.ToneError)))
			}
			if firstErr < 0 {
				firstErr = len(body)
			}
			body = append(body, pluginsdk.Line(pluginsdk.Span(l, protocol.ToneError)))
		case strings.HasPrefix(trimmed, "##[warning]"):
			body = append(body, pluginsdk.Line(pluginsdk.Span("▲ "+text, protocol.ToneWarn)))
		default:
			body = append(body, pluginsdk.Line(pluginsdk.Span(l)))
		}
	}
	title := "── output of the failed jobs"
	if firstErr >= 0 {
		start := 0
		for _, at := range steps {
			if at <= firstErr {
				start = at
			}
		}
		if start > 0 {
			body = body[start:]
			title = "── output of the step that failed"
		}
	}
	for len(body) > 0 && body[0] == nil {
		body = body[1:]
	}
	body = foldAlike(body)
	var out2 [][]protocol.Span
	if len(errs) > 0 {
		out2 = append(out2, nil, pluginsdk.Line(protocol.Span{Text: "── errors", Tone: protocol.ToneError, Bold: true}))
		out2 = append(out2, errs...)
	}
	if len(body) > maxOutputTail {
		body = body[len(body)-maxOutputTail:]
		title += fmt.Sprintf(" · the last %d lines", maxOutputTail)
	}
	out2 = append(out2, nil, pluginsdk.Line(protocol.Span{Text: title, Tone: protocol.ToneMuted, Bold: true}))
	return append(out2, body...)
}

// alikePrefix is how much of the start two lines share to be taken for the same message over and over, and
// alikeRun how many of them in a row are folded
const (
	alikePrefix = 40
	alikeRun    = 5
)

// foldAlike keeps the first of a run of lines that start the same (a tool warning about each of many files) and says
// how many more there were.
func foldAlike(lines [][]protocol.Span) [][]protocol.Span {
	key := func(l []protocol.Span) string {
		t := []rune(strings.TrimSpace(textOf(l)))
		if len(t) < alikePrefix {
			return ""
		}
		return string(t[:alikePrefix])
	}
	var out [][]protocol.Span
	for i := 0; i < len(lines); {
		k, j := key(lines[i]), i+1
		for k != "" && j < len(lines) && key(lines[j]) == k {
			j++
		}
		out = append(out, lines[i])
		if n := j - i - 1; n >= alikeRun-1 {
			out = append(out, pluginsdk.Line(pluginsdk.Span(fmt.Sprintf("  … and %d more like it", n), protocol.ToneMuted)))
		} else {
			out = append(out, lines[i+1:j]...)
		}
		i = j
	}
	return out
}

func textOf(l []protocol.Span) string {
	var b strings.Builder
	for _, s := range l {
		b.WriteString(s.Text)
	}
	return b.String()
}

func stepTone(status string) protocol.Tone {
	if status == "failure" {
		return protocol.ToneError
	}
	return ""
}

func (a *app) workflowOptions(r pluginsdk.OptionsRequest) ([]protocol.Option, error) {
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return nil, err
	}
	wfs, err := repo.Forge.Workflows(repo)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.Option, len(wfs))
	for i, w := range wfs {
		out[i] = protocol.Option{Value: w.ID, Label: w.Name}
	}
	return out, nil
}

// branchOptions are the branches of the repository, the one of the service first.
func (a *app) branchOptions(r pluginsdk.OptionsRequest) ([]protocol.Option, error) {
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return nil, err
	}
	names, err := repo.Forge.Branches(repo)
	if err != nil {
		return nil, err
	}
	var out []protocol.Option
	if repo.Branch != "" {
		out = append(out, protocol.Option{Value: repo.Branch, Label: repo.Branch + " (this service)"})
	}
	for _, n := range names {
		if n != repo.Branch {
			out = append(out, protocol.Option{Value: n, Label: n})
		}
	}
	return out, nil
}

// startRun runs CI on a branch now, or schedules it for when the user said.
func (a *app) startRun(r pluginsdk.ActionRequest) (*protocol.InvokeResult, error) {
	repo, err := a.repoOf(r.Context.Service)
	if err != nil {
		return nil, err
	}
	branch := cmp.Or(r.Inputs["branch"], repo.Branch)
	if branch == "" {
		return nil, errors.New("which branch?")
	}
	workflow := r.Inputs["workflow"]
	name := cmp.Or(workflow, "CI")
	if wfs, err := repo.Forge.Workflows(repo); err == nil {
		for _, w := range wfs {
			if w.ID == workflow {
				name = w.Name
			}
		}
	}
	now := a.now()
	at, err := parseWhen(r.Inputs["when"], now)
	if err != nil {
		return nil, err
	}
	if at.Sub(now) < time.Minute {
		if err := repo.Forge.Start(repo, workflow, branch); err != nil {
			return nil, err
		}
		a.afterAction()
		return pluginsdk.Done(protocol.ToneOK, fmt.Sprintf("started %s on %s", name, branch), "ci"), nil
	}
	x, err := a.sched.add(Scheduled{Root: repo.Root, Slug: repo.Slug, Workflow: workflow, Name: name, Branch: branch, At: at})
	if err != nil {
		return nil, fmt.Errorf("cannot keep the schedule: %w", err)
	}
	return pluginsdk.Done(protocol.ToneInfo, fmt.Sprintf("%s on %s will run %s (%s)", x.Name, x.Branch, until(x.At, now), x.At.Format("Mon 15:04")), "ci"), nil
}

// runSchedules starts the scheduled runs when their time comes.
func (a *app) runSchedules() {
	a.fireDue()
	for range time.Tick(15 * time.Second) {
		a.fireDue()
	}
}

func (a *app) fireDue() {
	start, missed := a.sched.due()
	for _, x := range missed {
		a.n.Toast(protocol.ToneWarn, fmt.Sprintf("%s on %s was due %s while orbit was closed: not run", x.Name, x.Branch, x.At.Format("Mon 15:04")))
	}
	for _, x := range start {
		repo, err := a.repos.of(x.Root)
		if err == nil {
			err = repo.Forge.Start(repo, x.Workflow, x.Branch)
		}
		if err != nil {
			a.n.Toast(protocol.ToneError, fmt.Sprintf("scheduled %s on %s: %v", x.Name, x.Branch, err))
			continue
		}
		a.n.Toast(protocol.ToneOK, fmt.Sprintf("started %s on %s, as scheduled", x.Name, x.Branch))
	}
	if len(start)+len(missed) > 0 {
		a.n.Invalidate("ci", "")
		go a.refresh()
	}
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
		if isScheduled(id) {
			return nil, errors.New("it has not run yet: C cancels it")
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
	if isScheduled(id) {
		return nil, errors.New("it has not run yet: it has no page")
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
