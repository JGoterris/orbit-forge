package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/JGoterris/orbit/pluginsdk"
	"github.com/JGoterris/orbit/pluginsdk/protocol"
)

// clocked puts the cache of the app on a clock the test moves.
func clocked(a *app) *time.Time {
	now := time.Now()
	a.cache.now = func() time.Time { return now }
	return &now
}

func count(f *fakeForge, prefix string) int {
	n := 0
	for _, c := range f.did() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestWhatWasSeenIsNotAskedAgainWhileItIsFresh(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs()}
	a, _ := newTestApp(t, f)
	clocked(a)
	for range 3 {
		if _, err := a.mrInfo(req("api", map[string]string{"list": "41"})); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(f, "ViewMR"); n != 1 {
		t.Fatalf("asked %d times", n)
	}
}

func TestWhatIsStaleShowsAtOnceAndIsAskedAgainBehindTheScenes(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs()}
	a, rec := newTestApp(t, f)
	now := clocked(a)
	r := req("api", map[string]string{"list": "41"})
	a.mrInfo(r)

	// nothing changed: asked again, nobody told
	*now = now.Add(freshFor + time.Second)
	a.mrInfo(r)
	if n := count(f, "ViewMR"); n != 2 || rec.invalid != 0 {
		t.Fatalf("asked %d times, orbit told %d times", n, rec.invalid)
	}

	// it changed: the old one is shown now, and orbit is told to look again
	f.mrs[0].Title = "Fix logout"
	*now = now.Add(freshFor + time.Second)
	d, _ := a.mrInfo(r)
	if rec.invalid != 1 || strings.Contains(sectionsText(d), "Fix logout") {
		t.Fatalf("told %d times:\n%s", rec.invalid, sectionsText(d))
	}
	d, _ = a.mrInfo(r)
	if !strings.Contains(sectionsText(d), "Fix logout") {
		t.Fatalf("the new one is not shown:\n%s", sectionsText(d))
	}
}

func sectionsText(d protocol.ViewData) string {
	var b strings.Builder
	for _, s := range d.Detail.Sections {
		b.WriteString(s.Title + "\n")
	}
	return b.String()
}

func TestAFailedLookBehindTheScenesKeepsWhatWasKnown(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs()}
	a, rec := newTestApp(t, f)
	now := clocked(a)
	a.mrList(req("api", nil))
	f.err = errNoRepo
	*now = now.Add(freshFor + time.Second)
	d, err := a.mrList(req("api", nil))
	if err != nil || len(d.List.Items) != 2 || rec.invalid != 0 {
		t.Fatalf("%v %d items, told %d times", err, len(d.List.Items), rec.invalid)
	}
}

func TestTheJobsOfAFinishedRunAreAskedOnce(t *testing.T) {
	f := &fakeForge{kind: "github", jobs: []Job{{ID: "1", Name: "build", Status: "success"}}}
	a, _ := newTestApp(t, f)
	now := clocked(a)
	for range 3 {
		a.ciGraph(req("api", map[string]string{"list": "7"}))
		*now = now.Add(time.Hour)
	}
	if n := count(f, "Jobs"); n != 1 {
		t.Fatalf("asked %d times", n)
	}
}

func TestTheJobsOfARunThatGoesOnAreAskedAgain(t *testing.T) {
	f := &fakeForge{kind: "github", jobs: []Job{{ID: "1", Name: "build", Status: "running"}}}
	a, rec := newTestApp(t, f)
	now := clocked(a)
	r := req("api", map[string]string{"list": "7"})
	a.ciGraph(r)
	f.jobs = []Job{{ID: "1", Name: "build", Status: "success"}}
	*now = now.Add(freshFor + time.Second)
	a.ciGraph(r)
	if rec.invalid != 1 || count(f, "Jobs") != 2 {
		t.Fatalf("told %d times, asked %d", rec.invalid, count(f, "Jobs"))
	}
	a.ciGraph(r)
	*now = now.Add(time.Hour)
	a.ciGraph(r)
	if count(f, "Jobs") != 2 {
		t.Fatal("it finished: it is not asked any more")
	}
}

func TestAnActionMakesTheNextLookGoToTheForge(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs()}
	a, _ := newTestApp(t, f)
	clocked(a)
	sel := map[string]string{"list": "41"}
	a.mrInfo(req("api", sel))
	a.mrList(req("api", nil))
	if _, err := a.act("mr-merge", pluginsdk.ActionRequest{Context: protocol.Context{Service: "api", Selection: sel}}); err != nil {
		t.Fatal(err)
	}
	f.reset()
	a.mrInfo(req("api", sel))
	a.mrList(req("api", nil))
	if !slices.Equal(f.did(), []string{"ViewMR 41", "ListMRs"}) {
		t.Fatalf("%v", f.did())
	}
}

func TestARunThatChangedForgetsItsJobs(t *testing.T) {
	f := &fakeForge{kind: "github", runs: []Run{{ID: "7", Name: "CI", Branch: "feature", Status: "failure"}}, jobs: []Job{{ID: "1", Name: "build", Status: "failure"}}}
	a, rec := newTestApp(t, f)
	now := clocked(a)
	a.ciList(req("api", nil))
	a.ciGraph(req("api", map[string]string{"list": "7"}))
	f.runs[0].Status = "running"
	f.jobs = []Job{{ID: "1", Name: "build", Status: "running"}}
	*now = now.Add(freshFor + time.Second)
	a.ciList(req("api", nil))
	f.reset()
	a.ciGraph(req("api", map[string]string{"list": "7"}))
	if rec.invalid == 0 || count(f, "Jobs") != 1 {
		t.Fatalf("told %d times, asked %d", rec.invalid, count(f, "Jobs"))
	}
}

func TestThePollTellsWhenTheListChanged(t *testing.T) {
	f := &fakeForge{kind: "github", mrs: mrs()}
	a, rec := newTestApp(t, f)
	a.refresh()
	before := rec.invalid
	a.refresh()
	if rec.invalid != before {
		t.Fatal("nothing changed")
	}
	f.mrs = f.mrs[:1]
	a.refresh()
	d, _ := a.mrList(req("api", nil))
	if len(d.List.Items) != 1 || count(f, "ListMRs") != 3 {
		t.Fatalf("%d items, asked %d", len(d.List.Items), count(f, "ListMRs"))
	}
}
