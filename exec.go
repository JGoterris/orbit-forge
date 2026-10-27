package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const toolTimeout = 30 * time.Second

// run executes a command in dir and returns what it printed. The error carries the first line of what it said on
// stderr, which is what gh and glab explain themselves with.
func run(dir, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "CLICOLOR=0", "GH_PROMPT_DISABLED=1", "GIT_TERMINAL_PROMPT=0", "GLAB_NO_PROMPT=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%s is not installed", name)
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s did not answer in %s", name, toolTimeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		first, _, _ := strings.Cut(msg, "\n")
		return nil, fmt.Errorf("%s: %s", name, first)
	}
	return stdout.Bytes(), nil
}

func gitOutput(dir string, args ...string) (string, error) {
	out, err := run(dir, "git", args...)
	return strings.TrimSpace(string(out)), err
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)

// plain drops the colour codes the CI logs of GitLab are full of.
func plain(s string) string { return ansiRe.ReplaceAllString(strings.ReplaceAll(s, "\r", ""), "") }

func tail(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(plain(s), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot open the browser: %w", err)
	}
	go cmd.Wait()
	return nil
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
}
