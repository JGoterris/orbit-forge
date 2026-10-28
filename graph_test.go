package main

import (
	"strings"
	"testing"
	"time"

	"github.com/JGoterris/orbit/pluginsdk/protocol"
)

func render(lines [][]protocol.Span) string {
	var b strings.Builder
	for _, l := range lines {
		for _, s := range l {
			b.WriteString(s.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func at(min, sec int) time.Time {
	return time.Date(2026, 10, 28, 10, min, sec, 0, time.UTC)
}

// a workflow as GitHub would run it: two jobs to start, a matrix after both, and a deploy after the tests and lint
func sampleJobs() []Job {
	return []Job{
		{ID: "1", Name: "build", Status: "success", Needs: []string{}, Started: at(0, 0), Finished: at(1, 2)},
		{ID: "2", Name: "lint", Status: "success", Needs: []string{}, Started: at(0, 0), Finished: at(0, 22)},
		{ID: "3", Name: "test (linux)", Status: "success", Needs: []string{"build"}, Started: at(1, 5), Finished: at(3, 15)},
		{ID: "4", Name: "test (macos)", Status: "failure", Needs: []string{"build"}, Started: at(1, 5), Finished: at(4, 0)},
		{ID: "5", Name: "docs", Status: "success", Needs: []string{"build"}},
		{ID: "6", Name: "deploy", Status: "skipped", Needs: []string{"test (linux)", "test (macos)", "lint"}},
	}
}

func TestGraphJoinsEachJobToWhatItNeeds(t *testing.T) {
	got := render(graphLines(sampleJobs(), 200))
	want := `✓ build 1m02s ───┬─▶ ✓ test (linux) 2m10s ─┬─▶ ⊘ deploy
✓ lint    22s ─┐ ├─▶ ✖ test (macos) 2m55s ─┤
               │ └─▶ ✓ docs                │
               └───▶ ──────────────────────┘
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestGraphKeepsApartWhatIsNotJoined(t *testing.T) {
	jobs := []Job{
		{ID: "1", Name: "a", Status: "success", Needs: []string{}},
		{ID: "2", Name: "b", Status: "success", Needs: []string{}},
		{ID: "3", Name: "a2", Status: "success", Needs: []string{"a"}},
		{ID: "4", Name: "b2", Status: "success", Needs: []string{"b"}},
		{ID: "5", Name: "end", Status: "success", Needs: []string{"a"}},
	}
	got := render(graphLines(jobs, 200))
	want := `✓ a    ───┬─▶ ✓ a2
✓ b    ─┐ └─▶ ✓ end
        └───▶ ✓ b2
`
	if got != want {
		t.Fatalf("b must not look joined to a:\n%s", got)
	}
}

func TestGraphOfGitLabStages(t *testing.T) {
	jobs := []Job{
		{ID: "74", Name: "deploy", Status: "queued", Stage: "deploy"},
		{ID: "71", Name: "build", Status: "success", Stage: "build"},
		{ID: "72", Name: "test", Status: "failure", Stage: "test"},
		{ID: "73", Name: "lint", Status: "success", Stage: "test"},
	}
	got := render(graphLines(jobs, 200))
	lines := strings.Split(got, "\n")
	if !strings.HasPrefix(lines[0], "build") || !strings.Contains(lines[0], "test") || !strings.Contains(lines[0], "deploy") {
		t.Fatalf("the stages name the columns:\n%s", got)
	}
	if !strings.Contains(got, "✓ build  ─┬─▶ ✖ test  ─┬─▶ ◌ deploy") || !strings.Contains(got, "└─▶ ✓ lint  ─┘") {
		t.Fatalf("each job waits for the stage before its own:\n%s", got)
	}
	// with needs, a job goes as soon as what it needs is done, whatever its stage
	jobs[0].Needs = []string{"build"}
	got = render(graphLines(jobs, 200))
	if !strings.Contains(got, "◌ deploy") || strings.Count(strings.Split(got, "\n")[2], "▶") != 1 {
		t.Fatalf("deploy needs only build:\n%s", got)
	}
}

func TestGraphOfANarrowPaneIsAList(t *testing.T) {
	got := render(graphLines(sampleJobs(), 40))
	for _, want := range []string{"── 1", "✓ build  1m02s", "✖ test (macos)  2m55s  ← build", "⊘ deploy  ← test (linux), test (macos), lint"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lacks %q:\n%s", want, got)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "▶") {
			t.Fatalf("no lines in a list:\n%s", got)
		}
	}
}

func TestGraphDoesNotHangOnALoop(t *testing.T) {
	jobs := []Job{{ID: "1", Name: "a", Needs: []string{"b"}}, {ID: "2", Name: "b", Needs: []string{"a"}}}
	if len(graphLines(jobs, 100)) == 0 {
		t.Fatal("something must be drawn")
	}
}

func TestLinkNeedsFromTheWorkflow(t *testing.T) {
	wf, err := parseWorkflow([]byte(`
on: push
jobs:
  build:
    runs-on: ubuntu-latest
  test:
    name: Test
    needs: build
    strategy: { matrix: { os: [linux, macos] } }
  release:
    needs: [test, build]
    uses: ./.github/workflows/release.yml
  notify:
    name: Notify ${{ github.actor }}
    needs: release
`))
	if err != nil {
		t.Fatal(err)
	}
	jobs := []Job{{Name: "build"}, {Name: "Test (linux)"}, {Name: "Test (macos)"}, {Name: "release / publish"}, {Name: "Notify ana"}, {Name: "stray"}}
	linkNeeds(jobs, wf)
	want := map[string]string{
		"build": "", "Test (linux)": "build", "Test (macos)": "build",
		"release / publish": "Test (linux),Test (macos),build", "Notify ana": "release / publish",
	}
	for _, j := range jobs {
		if j.Name == "stray" {
			if j.Needs != nil {
				t.Errorf("a job the workflow does not know needs nothing known: %v", j.Needs)
			}
			continue
		}
		if j.Needs == nil || strings.Join(j.Needs, ",") != want[j.Name] {
			t.Errorf("%s needs %v, want %q", j.Name, j.Needs, want[j.Name])
		}
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "", 9 * time.Second: "9s", 62 * time.Second: "1m02s", 3*time.Hour + 5*time.Minute: "3h05m"} {
		if got := shortDuration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
