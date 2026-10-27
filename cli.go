package main

import (
	"fmt"
	"os"
	"strings"
)

const usage = `usage: orbit forge mr | ci [--all]

  mr    the open merge requests of the repository you are in
  ci    the latest runs of CI of its current branch (--all: of every branch)
`

func cli(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return 0
	}
	all := false
	for _, a := range args[1:] {
		if a == "--all" {
			all = true
		}
	}
	dir, _ := os.Getwd()
	repo, err := newRepos().of(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "orbit forge:", err)
		return 1
	}
	switch args[0] {
	case "mr":
		mrs, err := repo.Forge.ListMRs(repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "orbit forge:", err)
			return 1
		}
		fmt.Printf("%s · %s\n", repo.Slug, repo.Forge.Kind())
		if len(mrs) == 0 {
			fmt.Println("no open merge requests")
		}
		for _, m := range mrs {
			fmt.Printf("%-7s %-6s %-8s %-12s %s\n", repo.Forge.Prefix()+m.ID, checkText[m.Checks], m.Review, m.Author, m.Title)
		}
	case "ci":
		branch := repo.Branch
		if all {
			branch = ""
		}
		runs, err := repo.Forge.ListRuns(repo, branch)
		if err != nil {
			fmt.Fprintln(os.Stderr, "orbit forge:", err)
			return 1
		}
		fmt.Printf("%s · %s\n", repo.Slug, map[bool]string{true: "all branches", false: "branch " + repo.Branch}[all || repo.Branch == ""])
		if len(runs) == 0 {
			fmt.Println("no runs")
		}
		for _, r := range runs {
			fmt.Printf("%-9s %-5s %-20s %s\n", r.Status, ago(r.Created), r.Branch, strings.TrimSpace(r.Name+" · "+r.Title))
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		return 1
	}
	return 0
}
