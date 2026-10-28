package main

import (
	"html"
	"regexp"
	"strings"

	"github.com/JGoterris/orbit/pluginsdk"
	"github.com/JGoterris/orbit/pluginsdk/protocol"
)

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->[ \t]*\n?`)
	// inline are the marks of a line that change how it looks: `code`, **bold**, __bold__
	inline = regexp.MustCompile("`[^`]+`|\\*\\*[^*]+\\*\\*|__[^_]+__")
	mdLink = regexp.MustCompile(`!?\[([^\]]*)\]\(([^)\s]+)[^)]*\)`)

	// the HTML that bots (dependabot, renovate) and some templates write, made into the markdown it stands for
	htmlBlocks = []struct {
		re   *regexp.Regexp
		with string
	}{
		{regexp.MustCompile(`(?is)<a\s[^>]*>(.*?)</a>`), "$1"},
		{regexp.MustCompile(`(?i)<br\s*/?>`), "\n"},
		{regexp.MustCompile(`(?i)<h[1-6][^>]*>`), "\n### "},
		{regexp.MustCompile(`(?i)<li[^>]*>`), "\n- "},
		{regexp.MustCompile(`(?i)<summary[^>]*>`), "\n### ▸ "},
		{regexp.MustCompile(`(?i)</?pre[^>]*>`), "\n```\n"},
		{regexp.MustCompile(`(?i)</?code[^>]*>`), "`"},
		{regexp.MustCompile(`(?i)</?(strong|b)>`), "**"},
		{regexp.MustCompile(`(?i)</(p|div|h[1-6]|ul|ol|tr|summary|details|blockquote|table)>|<(p|div|ul|ol|details|blockquote|table)[^>]*>`), "\n"},
		{regexp.MustCompile(`<[a-zA-Z/][^>]*>`), ""},
	}
)

// fromHTML turns the HTML tags of a text into markdown, and its entities into what they stand for.
func fromHTML(body string) string {
	if !strings.Contains(body, "<") && !strings.Contains(body, "&") {
		return body
	}
	for _, b := range htmlBlocks {
		body = b.re.ReplaceAllString(body, b.with)
	}
	return strings.ReplaceAll(html.UnescapeString(body), "\u200b", "")
}

// markdown turns the text of a description or a comment into lines orbit can draw: headings in bold, lists with a
// bullet, quotes and code dimmed, links as their text and address. The comments of templates go; the rest stays.
func markdown(body string, base protocol.Tone) [][]protocol.Span {
	body = fromHTML(htmlComment.ReplaceAllString(plain(body), ""))
	var out [][]protocol.Span
	code, blank := false, false
	for _, raw := range strings.Split(strings.TrimSpace(body), "\n") {
		line := strings.TrimRight(raw, " ")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			code = !code
			continue
		}
		if code {
			out = append(out, pluginsdk.Line(pluginsdk.Span("  "+line, protocol.ToneInfo)))
			blank = false
			continue
		}
		if trimmed == "" {
			// one empty line between paragraphs is enough
			if !blank && len(out) > 0 {
				out = append(out, nil)
			}
			blank = true
			continue
		}
		blank = false
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		switch {
		case strings.HasPrefix(trimmed, "#"):
			text := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			out = append(out, []protocol.Span{{Text: text, Bold: true}})
		case trimmed == "---" || trimmed == "***" || trimmed == "___":
			out = append(out, pluginsdk.Line(pluginsdk.Span("───", protocol.ToneMuted)))
		case strings.HasPrefix(trimmed, "> "):
			out = append(out, append([]protocol.Span{pluginsdk.Span(indent+"│ ", protocol.ToneMuted)}, spans(trimmed[2:], protocol.ToneMuted)...))
		case strings.HasPrefix(trimmed, "- [ ] "), strings.HasPrefix(trimmed, "* [ ] "):
			out = append(out, append([]protocol.Span{pluginsdk.Span(indent+"☐ ", protocol.ToneMuted)}, spans(trimmed[6:], base)...))
		case strings.HasPrefix(strings.ToLower(trimmed), "- [x] "), strings.HasPrefix(strings.ToLower(trimmed), "* [x] "):
			out = append(out, append([]protocol.Span{pluginsdk.Span(indent+"☑ ", protocol.ToneOK)}, spans(trimmed[6:], base)...))
		case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "), strings.HasPrefix(trimmed, "+ "):
			out = append(out, append([]protocol.Span{pluginsdk.Span(indent+"• ", protocol.ToneMuted)}, spans(trimmed[2:], base)...))
		default:
			out = append(out, spans(line, base))
		}
	}
	for len(out) > 0 && out[len(out)-1] == nil {
		out = out[:len(out)-1]
	}
	return out
}

// spans is a line with its inline marks: `code` in another colour, **bold** in bold, [text](url) as text and url.
func spans(line string, base protocol.Tone) []protocol.Span {
	line = mdLink.ReplaceAllStringFunc(line, func(m string) string {
		sub := mdLink.FindStringSubmatch(m)
		switch {
		case sub[1] == "":
			return sub[2]
		case strings.Contains(sub[2], strings.TrimPrefix(strings.TrimPrefix(sub[1], "https://"), "http://")):
			return sub[1] // the address only says the text again
		}
		return sub[1] + " (" + sub[2] + ")"
	})
	var out []protocol.Span
	at := 0
	for _, loc := range inline.FindAllStringIndex(line, -1) {
		if loc[0] > at {
			out = append(out, pluginsdk.Span(line[at:loc[0]], base))
		}
		m := line[loc[0]:loc[1]]
		if strings.HasPrefix(m, "`") {
			out = append(out, pluginsdk.Span(strings.Trim(m, "`"), protocol.ToneInfo))
		} else {
			out = append(out, protocol.Span{Text: m[2 : len(m)-2], Tone: base, Bold: true})
		}
		at = loc[1]
	}
	if at < len(line) {
		out = append(out, pluginsdk.Span(line[at:], base))
	}
	return out
}
