package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Scheduled is a run of CI asked for later: the plugin starts it when its time comes, while orbit is open.
type Scheduled struct {
	ID       string    `json:"id"`
	Root     string    `json:"root"` // the repository
	Slug     string    `json:"slug"`
	Workflow string    `json:"workflow"`
	Name     string    `json:"name"` // of the workflow, to show
	Branch   string    `json:"branch"`
	At       time.Time `json:"at"`
}

// missedAfter is how late a schedule may still start: one whose time passed while orbit was closed for longer is
// dropped, not run at a moment nobody chose.
const missedAfter = 15 * time.Minute

// schedules keeps the scheduled runs in a file of the state of the plugin, which every orbit open shares: each run
// is claimed before it starts, so only one of them starts it.
type schedules struct {
	mu   sync.Mutex
	dir  string
	list []Scheduled
	now  func() time.Time
}

func newSchedules(dir string) *schedules {
	s := &schedules{dir: dir, now: time.Now}
	s.load()
	return s
}

func (s *schedules) file() string { return filepath.Join(s.dir, "schedules.json") }

// load reads the file again: another orbit may have changed it. Call with mu held, or before anyone else can.
func (s *schedules) load() {
	s.list = nil
	if s.dir == "" {
		return
	}
	if b, err := os.ReadFile(s.file()); err == nil {
		_ = json.Unmarshal(b, &s.list)
	}
}

func (s *schedules) save() error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s.list, "", "  ")
	tmp := s.file() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.file())
}

func (s *schedules) add(x Scheduled) (Scheduled, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var b [4]byte
	_, _ = rand.Read(b[:])
	x.ID = "at-" + hex.EncodeToString(b[:])
	s.list = append(s.list, x)
	slices.SortStableFunc(s.list, func(a, b Scheduled) int { return a.At.Compare(b.At) })
	return x, s.save()
}

func (s *schedules) remove(id string) (Scheduled, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	i := slices.IndexFunc(s.list, func(x Scheduled) bool { return x.ID == id })
	if i < 0 {
		return Scheduled{}, false, nil
	}
	x := s.list[i]
	s.list = slices.Delete(s.list, i, i+1)
	return x, true, s.save()
}

// of are the scheduled runs of a repository, the soonest first.
func (s *schedules) of(root string) []Scheduled {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var out []Scheduled
	for _, x := range s.list {
		if x.Root == root {
			out = append(out, x)
		}
	}
	return out
}

// due takes out of the list the runs whose time came, the ones to start and the ones that were missed, each claimed
// by this process.
func (s *schedules) due() (start, missed []Scheduled) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	now := s.now()
	var keep []Scheduled
	for _, x := range s.list {
		switch {
		case x.At.After(now):
			keep = append(keep, x)
		case !s.claim(x.ID):
			// another orbit took it
		case now.Sub(x.At) > missedAfter:
			missed = append(missed, x)
		default:
			start = append(start, x)
		}
	}
	if len(start)+len(missed) > 0 {
		s.list = keep
		_ = s.save()
	}
	return start, missed
}

// claim is true for the one process that creates the mark of a run; the marks of the last day are kept so that a
// late process does not start it again.
func (s *schedules) claim(id string) bool {
	if s.dir == "" {
		return true
	}
	dir := filepath.Join(s.dir, "claimed")
	_ = os.MkdirAll(dir, 0o755)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

var clockRe = regexp.MustCompile(`^(\d{1,2})[:.h](\d{2})$`)

// parseWhen reads when to run: "now", a delay ("30m", "1h30m", "in 2h"), a time of the day ("15:30", the next one
// to come), "tomorrow 9:00", or a date and a time ("2026-11-02 09:00").
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSpace(strings.TrimPrefix(s, "in "))
	if s == "" || s == "now" || s == "ya" || s == "ahora" {
		return now, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return time.Time{}, errors.New("that is in the past")
		}
		return now.Add(d), nil
	}
	clock := func(text string, day time.Time) (time.Time, bool) {
		m := clockRe.FindStringSubmatch(text)
		if m == nil {
			return time.Time{}, false
		}
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if h > 23 || min > 59 {
			return time.Time{}, false
		}
		return time.Date(day.Year(), day.Month(), day.Day(), h, min, 0, 0, now.Location()), true
	}
	if t, ok := clock(s, now); ok {
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	for _, word := range []string{"tomorrow ", "mañana "} {
		if rest, ok := strings.CutPrefix(s, word); ok {
			if t, ok := clock(strings.TrimSpace(rest), now.AddDate(0, 0, 1)); ok {
				return t, nil
			}
		}
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, now.Location()); err == nil {
		if !t.After(now) {
			return time.Time{}, errors.New("that is in the past")
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("when is %q? say now, 30m, 2h, 15:30, tomorrow 9:00 or 2026-11-02 09:00", s)
}

// until says how far away a moment is, for a list: "in 25m", "at 15:30", "tomorrow 09:00", "Mon 2 Nov 09:00".
func until(t, now time.Time) string {
	d := t.Sub(now)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()+0.5))
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return "at " + t.Format("15:04")
	case t.YearDay() == now.AddDate(0, 0, 1).YearDay():
		return "tomorrow " + t.Format("15:04")
	}
	return t.Format("Mon 2 Jan 15:04")
}
