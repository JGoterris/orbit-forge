// Command orbit-forge is a plugin for orbit: the merge requests and the CI of GitHub and GitLab, with gh and glab.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "orbit-forge:", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(cli(os.Args[1:]))
}
