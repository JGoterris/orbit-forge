package main

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/JGoterris/orbit/pluginsdk"
	"github.com/JGoterris/orbit/pluginsdk/protocol"
)

// workflowJob is a job of a GitHub workflow file: the name it shows with and the ids of the jobs it needs.
type workflowJob struct {
	Name  string
	Needs []string
}

// stringList is a YAML value that is a string or a list of them, as `needs:` is.
type stringList []string

func (s *stringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*s = []string{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}

func parseWorkflow(raw []byte) (map[string]workflowJob, error) {
	var wf struct {
		Jobs map[string]struct {
			Name  string     `yaml:"name"`
			Needs stringList `yaml:"needs"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("cannot read the workflow: %w", err)
	}
	out := make(map[string]workflowJob, len(wf.Jobs))
	for id, j := range wf.Jobs {
		out[id] = workflowJob{Name: j.Name, Needs: j.Needs}
	}
	return out, nil
}

// linkNeeds fills the Needs of the jobs of a run from its workflow. A job of the run is named after the name: of its
// job in the workflow (or its id), followed by its matrix ("test (linux, 1.22)") or by the job of a workflow it calls
// ("release / build"); a name with an expression is matched by what comes before it.
func linkNeeds(jobs []Job, wf map[string]workflowJob) {
	type label struct {
		id, text string
		prefix   bool
	}
	var labels []label
	for id, j := range wf {
		text, prefix := cmp.Or(j.Name, id), false
		if i := strings.Index(text, "${{"); i >= 0 {
			text, prefix = strings.TrimSpace(text[:i]), true
		}
		labels = append(labels, label{id, text, prefix})
	}
	// the longest first, so "build docs" is not taken for "build"
	slices.SortFunc(labels, func(a, b label) int {
		return cmp.Or(len(b.text)-len(a.text), strings.Compare(a.id, b.id))
	})
	idOf := func(name string) string {
		for _, l := range labels {
			if name == l.text {
				return l.id
			}
		}
		for _, l := range labels {
			if l.text == "" {
				continue
			}
			if strings.HasPrefix(name, l.text+" (") || strings.HasPrefix(name, l.text+" / ") || (l.prefix && strings.HasPrefix(name, l.text)) {
				return l.id
			}
		}
		return ""
	}
	byID := map[string][]string{}
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = idOf(j.Name)
		if ids[i] != "" {
			byID[ids[i]] = append(byID[ids[i]], j.Name)
		}
	}
	for i := range jobs {
		if ids[i] == "" {
			continue
		}
		jobs[i].Needs = []string{}
		for _, need := range wf[ids[i]].Needs {
			jobs[i].Needs = append(jobs[i].Needs, byID[need]...)
		}
	}
}

// node is a place in the graph: a job, or a piece of an edge that crosses a column (job < 0).
type node struct {
	job  int
	deps []int // the nodes of the previous column it comes from
}

// layout puts the jobs of a run in columns: a job goes right after the last of the jobs it needs. A job whose needs
// are not known needs the jobs of the stage before its own (GitLab without needs), or nothing.
func layout(jobs []Job) [][]node {
	if len(jobs) == 0 {
		return nil
	}
	index := map[string][]int{}
	for i, j := range jobs {
		index[j.Name] = append(index[j.Name], i)
	}
	// the stages, in the order of their first job (the ids grow as the pipeline goes)
	first := map[string]int{}
	for i, j := range jobs {
		if j.Stage == "" {
			continue
		}
		id, err := strconv.Atoi(j.ID)
		if err != nil {
			id = i
		}
		if f, ok := first[j.Stage]; !ok || id < f {
			first[j.Stage] = id
		}
	}
	stages := make([]string, 0, len(first))
	for s := range first {
		stages = append(stages, s)
	}
	slices.SortFunc(stages, func(a, b string) int { return first[a] - first[b] })

	deps := make([][]int, len(jobs))
	for i, j := range jobs {
		switch {
		case j.Needs != nil:
			for _, n := range j.Needs {
				deps[i] = append(deps[i], index[n]...)
			}
		case j.Stage != "":
			if at := slices.Index(stages, j.Stage); at > 0 {
				for k, o := range jobs {
					if o.Stage == stages[at-1] {
						deps[i] = append(deps[i], k)
					}
				}
			}
		}
	}
	level := make([]int, len(jobs))
	state := make([]int, len(jobs)) // 0 not seen, 1 on the way, 2 done
	var visit func(i int) int
	visit = func(i int) int {
		switch state[i] {
		case 1:
			return 0 // a loop: the forge would not run it, but do not hang on it
		case 2:
			return level[i]
		}
		state[i] = 1
		l := 0
		for _, d := range deps[i] {
			l = max(l, visit(d)+1)
		}
		state[i], level[i] = 2, l
		return l
	}
	depth := 0
	for i := range jobs {
		depth = max(depth, visit(i)+1)
	}
	cols := make([][]node, depth)
	at := make([]int, len(jobs)) // the position of each job in its column
	for i := range jobs {
		at[i] = len(cols[level[i]])
		cols[level[i]] = append(cols[level[i]], node{job: i})
	}
	// an edge that skips columns goes through a node of each column it crosses, one per job it comes from
	pass := map[[2]int]int{} // (job, column) → position of the node that carries it there
	for i := range jobs {
		n := &cols[level[i]][at[i]]
		for _, d := range dedupe(deps[i]) {
			from := at[d]
			for c := level[d] + 1; c < level[i]; c++ {
				key := [2]int{d, c}
				p, ok := pass[key]
				if !ok {
					p = len(cols[c])
					cols[c] = append(cols[c], node{job: -1, deps: []int{from}})
					pass[key] = p
				}
				from = p
			}
			if level[d] < level[i] {
				n.deps = append(n.deps, from)
			}
		}
	}
	order(cols)
	return cols
}

func dedupe(xs []int) []int {
	out := slices.Clone(xs)
	slices.Sort(out)
	return slices.Compact(out)
}

// order sorts each column by where its nodes come from, so the lines cross as little as they can.
func order(cols [][]node) {
	for c := 1; c < len(cols); c++ {
		col := cols[c]
		center := func(n node) float64 {
			if len(n.deps) == 0 {
				return 1e9
			}
			sum := 0
			for _, d := range n.deps {
				sum += d
			}
			return float64(sum) / float64(len(n.deps))
		}
		perm := make([]int, len(col))
		for i := range perm {
			perm[i] = i
		}
		slices.SortStableFunc(perm, func(a, b int) int { return cmp.Compare(center(col[a]), center(col[b])) })
		sorted := make([]node, len(col))
		moved := make([]int, len(col))
		for to, from := range perm {
			sorted[to] = col[from]
			moved[from] = to
		}
		cols[c] = sorted
		if c+1 < len(cols) {
			for k := range cols[c+1] {
				for x, d := range cols[c+1][k].deps {
					cols[c+1][k].deps[x] = moved[d]
				}
			}
		}
	}
}

func shortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

const maxNodeName = 26

func width(s string) int { return utf8.RuneCountInString(s) }

func cut(s string, w int) string {
	if width(s) <= w {
		return s
	}
	r := []rune(s)
	return string(r[:max(0, w-1)]) + "…"
}

// junctions are the box characters by the sides they join: up, down, left, right.
var junctions = map[[4]bool]string{
	{true, true, false, false}: "│", {false, false, true, true}: "─",
	{true, false, true, false}: "┘", {true, false, false, true}: "└", {false, true, true, false}: "┐", {false, true, false, true}: "┌",
	{true, true, true, false}: "┤", {true, true, false, true}: "├", {false, true, true, true}: "┬", {true, false, true, true}: "┴",
	{true, true, true, true}: "┼", {true, false, false, false}: "│", {false, true, false, false}: "│",
	{false, false, true, false}: "─", {false, false, false, true}: "─",
}

// graphLines draws the jobs of a run as GitHub and GitLab do: columns that run left to right, each job joined to the
// ones it needs. When that is wider than the pane, the jobs go down by column instead, each saying what it needs.
func graphLines(jobs []Job, avail int) [][]protocol.Span {
	cols := layout(jobs)
	if len(cols) == 0 {
		return nil
	}
	label := func(j Job) (name, dur string) {
		return cut(j.Name, maxNodeName), shortDuration(j.Duration())
	}
	colW := make([]int, len(cols))
	for c, col := range cols {
		colW[c] = 6
		for _, n := range col {
			if n.job >= 0 {
				name, dur := label(jobs[n.job])
				colW[c] = max(colW[c], 2+width(name)+1+width(dur))
			}
		}
	}
	// the lines between two columns: the groups of nodes that are joined each get a bus
	type gap struct {
		buses     int
		left      map[int]int // row of the left column → its bus
		right     map[int]int
		top, down []int
	}
	gaps := make([]gap, max(0, len(cols)-1))
	total := 0
	for c := range cols {
		total += colW[c]
	}
	for c := 0; c+1 < len(cols); c++ {
		g := gap{left: map[int]int{}, right: map[int]int{}}
		// union of the nodes joined by an edge, in rows of each side
		parent := map[int]int{}
		var find func(int) int
		find = func(x int) int {
			if p, ok := parent[x]; ok && p != x {
				r := find(p)
				parent[x] = r
				return r
			}
			parent[x] = x
			return x
		}
		const rightSide = 1 << 20
		for k, n := range cols[c+1] {
			for _, d := range n.deps {
				parent[find(d)] = find(rightSide + k)
			}
		}
		bus := map[int]int{}
		for k := range cols[c] {
			if _, ok := parent[k]; ok {
				r := find(k)
				if _, ok := bus[r]; !ok {
					bus[r] = len(bus)
				}
				g.left[k] = bus[r]
			}
		}
		for k := range cols[c+1] {
			if _, ok := parent[rightSide+k]; ok {
				g.right[k] = bus[find(rightSide+k)]
			}
		}
		g.buses = len(bus)
		arrangeBuses(g.left, g.right, g.buses)
		g.top, g.down = make([]int, g.buses), make([]int, g.buses)
		for b := range g.top {
			g.top[b], g.down[b] = 1<<30, -1
		}
		for row, b := range g.left {
			g.top[b], g.down[b] = min(g.top[b], row), max(g.down[b], row)
		}
		for row, b := range g.right {
			g.top[b], g.down[b] = min(g.top[b], row), max(g.down[b], row)
		}
		gaps[c] = g
		total += 4 + 2*g.buses
	}
	if avail > 0 && total > avail {
		return listLines(jobs, cols)
	}

	rows := 0
	for _, col := range cols {
		rows = max(rows, len(col))
	}
	var out [][]protocol.Span
	// the stages of GitLab name the columns, when a column is one stage
	var head []protocol.Span
	named := false
	for c, col := range cols {
		stage := ""
		for _, n := range col {
			if n.job < 0 {
				continue
			}
			if s := jobs[n.job].Stage; stage == "" || s == stage {
				stage = s
			} else {
				stage = ""
				break
			}
		}
		named = named || stage != ""
		head = append(head, pluginsdk.Span(fmt.Sprintf("%-*s", colW[c], cut(stage, colW[c])), protocol.ToneMuted))
		if c < len(gaps) {
			head = append(head, pluginsdk.Span(strings.Repeat(" ", 4+2*gaps[c].buses)))
		}
	}
	if named {
		out = append(out, head, nil)
	}
	for r := 0; r < rows; r++ {
		var line []protocol.Span
		for c, col := range cols {
			switch {
			case r >= len(col):
				line = append(line, pluginsdk.Span(strings.Repeat(" ", colW[c])))
			case col[r].job < 0:
				line = append(line, pluginsdk.Span(strings.Repeat("─", colW[c]), protocol.ToneMuted))
			default:
				j := jobs[col[r].job]
				name, dur := label(j)
				pad := colW[c] - 2 - width(name) - width(dur)
				line = append(line, pluginsdk.Span(jobGlyphs[j.Status]+" ", tone(runTones, j.Status)), pluginsdk.Span(name),
					pluginsdk.Span(strings.Repeat(" ", max(1, pad))+dur, protocol.ToneMuted))
			}
			if c >= len(gaps) {
				continue
			}
			g := gaps[c]
			lb, hasL := g.left[r]
			rb, hasR := g.right[r]
			var b strings.Builder
			switch {
			case hasL && col[r].job < 0:
				b.WriteString("──") // a line that crosses the column goes on without a break
			case hasL:
				b.WriteString(" ─")
			default:
				b.WriteString("  ")
			}
			for k := 0; k < g.buses; k++ {
				through := (hasL && lb > k) || (hasR && rb < k)
				left := (hasL && lb >= k) || (hasR && rb < k)
				right := (hasL && lb > k) || (hasR && rb <= k)
				if r >= g.top[k] && r <= g.down[k] {
					b.WriteString(junctions[[4]bool{r > g.top[k], r < g.down[k], left, right}])
				} else if through {
					b.WriteString("─")
				} else {
					b.WriteString(" ")
				}
				if (hasL && lb > k) || (hasR && rb <= k) {
					b.WriteString("─")
				} else {
					b.WriteString(" ")
				}
			}
			if hasR {
				b.WriteString("▶ ")
			} else {
				b.WriteString("  ")
			}
			line = append(line, pluginsdk.Span(b.String(), protocol.ToneMuted))
		}
		out = append(out, trimLine(line))
	}
	return out
}

// arrangeBuses orders the buses of a gap so that the lines cross as few of them as they can: a line that goes past
// a bus where it runs looks joined to it. left and right map a row to its bus, and are renumbered in place.
func arrangeBuses(left, right map[int]int, n int) {
	if n < 2 || n > 6 {
		return
	}
	span := make([][2]int, n)
	for b := range span {
		span[b] = [2]int{1 << 30, -1}
	}
	for _, m := range []map[int]int{left, right} {
		for row, b := range m {
			span[b] = [2]int{min(span[b][0], row), max(span[b][1], row)}
		}
	}
	cost := func(pos []int) int {
		c := 0
		rows := map[int]bool{}
		for r := range left {
			rows[r] = true
		}
		for r := range right {
			rows[r] = true
		}
		for r := range rows {
			lb, hasL := left[r]
			rb, hasR := right[r]
			for b := 0; b < n; b++ {
				if r < span[b][0] || r > span[b][1] {
					continue
				}
				if (hasL && pos[lb] > pos[b]) || (hasR && pos[rb] < pos[b]) {
					c++
				}
			}
		}
		return c
	}
	pos := make([]int, n)
	for i := range pos {
		pos[i] = i
	}
	best, bestCost := slices.Clone(pos), cost(pos)
	var permute func(k int)
	permute = func(k int) {
		if k == n {
			if c := cost(pos); c < bestCost {
				best, bestCost = slices.Clone(pos), c
			}
			return
		}
		for i := k; i < n; i++ {
			pos[k], pos[i] = pos[i], pos[k]
			permute(k + 1)
			pos[k], pos[i] = pos[i], pos[k]
		}
	}
	permute(0)
	for _, m := range []map[int]int{left, right} {
		for row, b := range m {
			m[row] = best[b]
		}
	}
}

// listLines are the jobs one under the other, by column, with what each needs: the graph of a narrow pane.
func listLines(jobs []Job, cols [][]node) [][]protocol.Span {
	var out [][]protocol.Span
	for c, col := range cols {
		title := fmt.Sprintf("── %d", c+1)
		if s := jobs[firstJob(col)].Stage; s != "" {
			title = "── " + s
		}
		if c > 0 {
			out = append(out, nil)
		}
		out = append(out, pluginsdk.Line(pluginsdk.Span(title, protocol.ToneMuted)))
		for _, n := range col {
			if n.job < 0 {
				continue
			}
			j := jobs[n.job]
			line := []protocol.Span{pluginsdk.Span(jobGlyphs[j.Status]+" ", tone(runTones, j.Status)), pluginsdk.Span(j.Name)}
			if d := shortDuration(j.Duration()); d != "" {
				line = append(line, pluginsdk.Span("  "+d, protocol.ToneMuted))
			}
			if len(j.Needs) > 0 {
				line = append(line, pluginsdk.Span("  ← "+strings.Join(j.Needs, ", "), protocol.ToneMuted))
			}
			out = append(out, line)
		}
	}
	return out
}

func firstJob(col []node) int {
	for _, n := range col {
		if n.job >= 0 {
			return n.job
		}
	}
	return 0
}

// trimLine drops the spaces at the end of a line, which only make it wider.
func trimLine(line []protocol.Span) []protocol.Span {
	for len(line) > 0 {
		last := &line[len(line)-1]
		last.Text = strings.TrimRight(last.Text, " ")
		if last.Text != "" {
			break
		}
		line = line[:len(line)-1]
	}
	return line
}
