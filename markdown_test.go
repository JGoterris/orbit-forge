package main

import (
	"strings"
	"testing"
	"time"

	"github.com/JGoterris/orbit/pluginsdk/protocol"
)

func TestMarkdownReadsLikeTheForgeShowsIt(t *testing.T) {
	lines := markdown("## Why\r\n<!-- say why -->\r\nFixes the **login** with `token`.\r\n\r\n\r\n- [x] tests\r\n- [ ] docs\r\n* see [the issue](https://x/1)\r\n> quoted\r\n\r\n```go\r\ncode   here\r\n```", "")
	got := render(lines)
	want := `Why
Fixes the login with token.

☑ tests
☐ docs
• see the issue (https://x/1)
│ quoted

  code   here
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if !lines[0][0].Bold {
		t.Error("a heading is bold")
	}
	var bold, code bool
	for _, s := range lines[1] {
		bold = bold || (s.Text == "login" && s.Bold)
		code = code || (s.Text == "token" && s.Tone == protocol.ToneInfo)
	}
	if !bold || !code {
		t.Errorf("inline marks: %+v", lines[1])
	}
}

func TestMarkdownLinkThatRepeatsItsTextIsTheText(t *testing.T) {
	got := render(markdown("[github.com/Microsoft/go-winio](https://github.com/Microsoft/go-winio) and [docs](https://x/d)", ""))
	if got != "github.com/Microsoft/go-winio and docs (https://x/d)\n" {
		t.Fatalf("got %q", got)
	}
}

func TestMarkdownOfTheHTMLOfBots(t *testing.T) {
	body := `Bumps <a href="https://x/winio">go-winio</a> from 0.6.3 to 0.6.4.
<details>
<summary>Release notes</summary>
<p><em>Sourced from <a href="https://x">releases</a>.</em></p>
<ul>
<li>Bump <code>x/sys</code> by <a href="https://x/d"><code>@​dependabot</code></a> in #393</li>
</ul>
</details>
Fixes &amp; more`
	got := render(markdown(body, ""))
	want := `Bumps go-winio from 0.6.3 to 0.6.4.

▸ Release notes

Sourced from releases.

• Bump x/sys by @dependabot in #393

Fixes & more
`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestMRSectionsSayAllThereIsToKnow(t *testing.T) {
	m := MR{ID: "41", Title: "Fix login", State: "open", Author: "ana", Branch: "fix", Target: "main",
		Additions: 120, Deletions: 30, Labels: []string{"bug"}, Milestone: "v2", AutoMerge: true, Created: time.Now().Add(-48 * time.Hour),
		Reviewers:  []Reviewer{{"bo", "approved"}, {"cy", "pending"}},
		FileList:   []File{{"login.go", 100, 20}, {"login_test.go", 20, 10}},
		CommitList: []Commit{{SHA: "abcdef1234", Title: "fix the token", Author: "ana"}},
		Notes:      []Note{{Author: "bo", Body: "Looks **good**", When: time.Now().Add(-time.Hour)}},
	}
	secs := mrSections("#", m)
	titles := []string{}
	for _, s := range secs {
		titles = append(titles, s.Title)
	}
	if strings.Join(titles, "|") != "#41 Fix login|Reviewers|Description|Files · 2 files|Commits · 1 commit|Conversation · 1 comment" {
		t.Fatalf("sections = %q", titles)
	}
	fields := map[string]string{}
	for _, f := range secs[0].Fields {
		fields[f.Label] = f.Value
	}
	if fields["changes"] != "+120 −30 · 2 files · 1 commit · 1 comment" || fields["opened"] != "2d ago" || fields["auto-merge"] != "on" || fields["milestone"] != "v2" {
		t.Fatalf("fields = %v", fields)
	}
	if len(secs[0].Badges) != 1 || secs[0].Badges[0].Text != "bug" {
		t.Fatalf("labels are badges: %+v", secs[0].Badges)
	}
	if secs[1].Fields[0].Tone != protocol.ToneOK || secs[1].Fields[1].Value != "pending" {
		t.Fatalf("reviewers = %+v", secs[1].Fields)
	}
	if got := render(secs[3].Text); !strings.Contains(got, "+100 −20  login.go") || !strings.Contains(got, "+20  −10  login_test.go") {
		t.Fatalf("files line up:\n%s", got)
	}
	if got := render(secs[4].Text); !strings.Contains(got, "abcdef1 fix the token  ana") {
		t.Fatalf("commits:\n%s", got)
	}
	if got := render(secs[5].Text); !strings.Contains(got, "bo · 1h ago") || !strings.Contains(got, "  Looks good") {
		t.Fatalf("conversation:\n%s", got)
	}
}
