package cmd

import "os"

// Colors holds ANSI escape codes for terminal output. Auto-disabled
// when stdout isn't a TTY or NO_COLOR is set in the environment.
type Colors struct {
	Reset  string
	Bold   string
	Dim    string
	Red    string
	Green  string
	Yellow string
	Blue   string
	Cyan   string
	Gray   string
}

// Fmt is the package-level color formatter. All output uses this; no
// hard-coded ANSI codes anywhere else.
var Fmt Colors

// Version is set from main.go at startup; defaults to "dev".
var Version = "dev"

func init() {
	if fi, _ := os.Stdout.Stat(); (fi.Mode()&os.ModeCharDevice) == 0 || os.Getenv("NO_COLOR") != "" {
		Fmt = Colors{}
		return
	}
	Fmt = Colors{
		Reset:  "\x1b[0m",
		Bold:   "\x1b[1m",
		Dim:    "\x1b[2m",
		Red:    "\x1b[31m",
		Green:  "\x1b[32m",
		Yellow: "\x1b[33m",
		Blue:   "\x1b[34m",
		Cyan:   "\x1b[36m",
		Gray:   "\x1b[90m",
	}
}
