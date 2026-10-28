package main

import (
	"strings"
	"testing"
	"time"

	"github.com/JGoterris/orbit/pluginsdk"
	"github.com/JGoterris/orbit/pluginsdk/protocol"
)

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 28, 14, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"":                 now,
		"now":              now,
		"30m":              now.Add(30 * time.Minute),
		"in 1h30m":         now.Add(90 * time.Minute),
		"15:30":            time.Date(2026, 10, 28, 15, 30, 0, 0, time.Local),
		"9:05":             time.Date(2026, 10, 29, 9, 5, 0, 0, time.Local), // already gone today: tomorrow
		"tomorrow 9:00":    time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local),
		"mañana 08.15":     time.Date(2026, 10, 29, 8, 15, 0, 0, time.Local),
		"2026-11-02 09:00": time.Date(2026, 11, 2, 9, 0, 0, 0, time.Local),
	} {
		got, err := parseWhen(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "25:00", "2020-01-01 10:00", "-5m"} {
		if _, err := parseWhen(bad, now); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if got := until(now.Add(25*time.Minute), now); got != "in 25m" {
		t.Errorf("until = %q", got)
	}
	if got := until(now.Add(20*time.Hour), now); got != "tomorrow 10:00" {
		t.Errorf("until = %q", got)
	}
}

func TestSchedulesAreSharedAndStartedOnce(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
	one, two := newSchedules(dir), newSchedules(dir)
	one.now, two.now = func() time.Time { return now }, func() time.Time { return now }
	x, err := one.add(Scheduled{Root: "/r", Workflow: "ci.yml", Name: "CI", Branch: "main", At: now.Add(time.Hour)})
	if err != nil || !isScheduled(x.ID) {
		t.Fatalf("%+v %v", x, err)
	}
	one.add(Scheduled{Root: "/other", At: now.Add(50 * time.Minute)})
	if got := two.of("/r"); len(got) != 1 || got[0].ID != x.ID {
		t.Fatalf("another orbit sees it: %+v", got)
	}
	if start, _ := one.due(); len(start) != 0 {
		t.Fatal("nothing is due yet")
	}
	now = now.Add(time.Hour)
	s1, _ := one.due()
	s2, _ := two.due()
	if len(s1)+len(s2) != 2 || len(two.of("/r")) != 0 {
		t.Fatalf("each run starts once, in one orbit: %d + %d", len(s1), len(s2))
	}
	if _, ok, _ := one.remove(x.ID); ok {
		t.Fatal("a run that started is not scheduled any more")
	}
}

func TestAScheduleMissedWhileClosedIsNotRun(t *testing.T) {
	s := newSchedules(t.TempDir())
	now := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.add(Scheduled{Root: "/r", Name: "CI", At: now.Add(-time.Hour)})
	s.add(Scheduled{Root: "/r", Name: "CI", At: now.Add(-5 * time.Minute)})
	start, missed := s.due()
	if len(start) != 1 || len(missed) != 1 {
		t.Fatalf("start %v missed %v", start, missed)
	}
}

func invoke(t *testing.T, a *app, action, sel string, inputs map[string]string) (*protocol.InvokeResult, error) {
	t.Helper()
	ctx := protocol.Context{Service: "api", Selection: map[string]string{}}
	if sel != "" {
		ctx.Selection["list"] = sel
	}
	return a.act(action, pluginsdk.ActionRequest{Context: ctx, Inputs: inputs})
}

func TestRunNowOrLater(t *testing.T) {
	f := &fakeForge{kind: "github"}
	a, rec := newTestApp(t, f)
	a.sched = newSchedules(t.TempDir())
	now := time.Date(2026, 10, 28, 14, 0, 0, 0, time.Local)
	a.now, a.sched.now = func() time.Time { return now }, func() time.Time { return now }

	res, err := invoke(t, a, "ci-run", "", map[string]string{"workflow": "ci.yml", "branch": "main", "when": "now"})
	if err != nil || res.Toast.Text != "started CI (ci.yml) on main" || f.did()[0] != "Start ci.yml main" {
		t.Fatalf("%+v %v %v", res, err, f.did())
	}
	f.reset()
	res, err = invoke(t, a, "ci-run", "", map[string]string{"workflow": "deploy.yml", "branch": "feature", "when": "15:30"})
	if err != nil || len(f.did()) != 0 || !strings.Contains(res.Toast.Text, "will run at 15:30") {
		t.Fatalf("a run for later starts nothing now: %+v %v %v", res, err, f.did())
	}
	d, _ := a.ciList(req("api", nil))
	first := d.List.Items[0]
	if !isScheduled(first.ID) || first.Cells["title"].Text != "⏲ Deploy (deploy.yml) · at 15:30" || first.Cells["event"].Text != "scheduled" {
		t.Fatalf("the scheduled run comes first in the list: %+v", first)
	}
	g, _ := a.ciGraph(req("api", map[string]string{"list": first.ID}))
	if !strings.Contains(render(g.Text.Lines), "branch feature") {
		t.Fatalf("its panes say what will run: %s", render(g.Text.Lines))
	}
	if _, err := invoke(t, a, "ci-rerun", first.ID, nil); err == nil {
		t.Fatal("a scheduled run cannot run again")
	}

	// its time comes
	now = now.Add(90 * time.Minute)
	f.reset()
	a.fireDue()
	if got := f.did(); len(got) != 1 || got[0] != "Start deploy.yml feature" || !strings.Contains(strings.Join(rec.toasts, "|"), "as scheduled") {
		t.Fatalf("calls %v toasts %v", got, rec.toasts)
	}

	// and one that is cancelled before
	invoke(t, a, "ci-run", "", map[string]string{"workflow": "ci.yml", "branch": "main", "when": "2h"})
	id := a.sched.of(sample)[0].ID
	if res, err := invoke(t, a, "ci-cancel", id, nil); err != nil || !strings.Contains(res.Toast.Text, "will not run") {
		t.Fatalf("%+v %v", res, err)
	}
	if len(a.sched.of(sample)) != 0 || strings.Contains(strings.Join(f.did(), "|"), "Cancel") {
		t.Fatalf("cancelling a schedule asks nothing of the forge: %v", f.did())
	}
	if _, err := invoke(t, a, "ci-run", "", map[string]string{"workflow": "ci.yml", "branch": "main", "when": "someday"}); err == nil {
		t.Fatal("a time that cannot be read is refused")
	}
}

func TestRunOptionsPutTheBranchOfTheServiceFirst(t *testing.T) {
	a, _ := newTestApp(t, &fakeForge{kind: "github"})
	opts, err := a.branchOptions(pluginsdk.OptionsRequest{Context: protocol.Context{Service: "api"}})
	if err != nil || len(opts) != 3 || opts[0].Value != "feature" || opts[1].Value != "main" {
		t.Fatalf("%+v %v", opts, err)
	}
	wfs, _ := a.workflowOptions(pluginsdk.OptionsRequest{Context: protocol.Context{Service: "api"}})
	if len(wfs) != 2 || wfs[0].Value != "ci.yml" || wfs[0].Label != "CI (ci.yml)" {
		t.Fatalf("%+v", wfs)
	}
}
