package main

import (
	"fmt"
	"os"

	"github.com/xdamman/odoo/cmd"
)

// VERSION is injected at release time via ldflags (-X main.VERSION=…).
// Falls back to "dev" otherwise.
var VERSION string

func main() {
	if VERSION != "" {
		cmd.Version = VERSION
	}

	args := os.Args[1:]
	if len(args) == 0 || cmd.HasFlag(args, "--help", "-h", "help") && firstNonFlag(args) == "" {
		cmd.PrintTopHelp()
		return
	}
	if cmd.HasFlag(args, "--version", "-V") {
		fmt.Printf("odoo %s\n", cmd.Version)
		return
	}

	// Top-level `--db <name>` flag is picked up by state.Active(). The
	// flag stays in args so individual commands can inspect / strip it
	// — most don't care.

	switch firstNonFlag(args) {
	case "setup":
		exitOn(cmd.Setup(stripCommand(args, "setup")))
	case "switch":
		exitOn(cmd.Switch(stripCommand(args, "switch")))
	case "journals":
		exitOn(cmd.Journals(stripCommand(args, "journals")))
	case "pull":
		exitOn(cmd.Pull(stripCommand(args, "pull")))
	case "push":
		exitOn(cmd.Push(stripCommand(args, "push")))
	case "sync":
		exitOn(cmd.Sync(stripCommand(args, "sync")))
	default:
		fmt.Fprintf(os.Stderr, "%sUnknown command: %s%s\n\n", cmd.Fmt.Red, firstNonFlag(args), cmd.Fmt.Reset)
		cmd.PrintTopHelp()
		os.Exit(1)
	}
}

// firstNonFlag returns the first arg that doesn't start with `-`. Used
// to find the command verb regardless of how the operator threaded
// global flags (`odoo --db prod journals` and `odoo journals --db prod`
// both resolve to "journals").
func firstNonFlag(args []string) string {
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		// Flags that consume a value — keep this list short; we only
		// need the global ones here. Sub-command flags are handled by
		// each command's own parsing.
		if a == "--db" {
			skip = true
			continue
		}
		if a == "-h" || a == "--help" || a == "--version" || a == "-V" {
			continue
		}
		if len(a) > 0 && a[0] == '-' {
			continue
		}
		return a
	}
	return ""
}

// stripCommand removes the FIRST occurrence of the given command verb
// from args, leaving everything else (including global flags) so the
// sub-command can re-parse `--db` / `--help` / etc.
func stripCommand(args []string, verb string) []string {
	out := make([]string, 0, len(args))
	stripped := false
	for _, a := range args {
		if !stripped && a == verb {
			stripped = true
			continue
		}
		out = append(out, a)
	}
	return out
}

func exitOn(err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "\n%s✗ %v%s\n\n", cmd.Fmt.Red, err, cmd.Fmt.Reset)
	os.Exit(1)
}
