package cmd

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Switch changes the active database. With a positional argument,
// switches directly. Without, prints the list and prompts for a pick.
//
// Usage:
//
//	odoo switch                # list + interactive pick (TTY)
//	odoo switch <name>         # direct switch
//	odoo switch --list         # list without changing anything
func Switch(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printSwitchHelp()
		return nil
	}

	names := ListDatabases()
	if len(names) == 0 {
		return fmt.Errorf("no database configured. Run `odoo setup` to add one")
	}
	state, _ := LoadState()
	listOnly := HasFlag(args, "--list")
	target := FirstPositional(args, "--db")

	if target == "" && !listOnly {
		// Show the list and prompt.
		printSwitchList(names, state)
		if !isTTY() {
			return fmt.Errorf("`odoo switch` without a name needs a TTY (or pass the name as an argument / use --list)")
		}
		r := bufio.NewReader(os.Stdin)
		fmt.Print("\n  Pick a database (name or number): ")
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		target = strings.TrimSpace(line)
		if target == "" {
			fmt.Println("  cancelled.")
			return nil
		}
		// Numeric pick → resolve to name.
		if n, err := strconvAtoi(target); err == nil && n >= 1 && n <= len(names) {
			target = names[n-1]
		}
	}

	if listOnly {
		printSwitchList(names, state)
		return nil
	}

	if _, err := os.Stat(DatabaseEnvPath(target)); err != nil {
		return fmt.Errorf("database %q not configured (available: %s)", target, strings.Join(names, ", "))
	}
	// Validate the .env actually loads + has all required fields.
	if _, err := LoadDatabase(target); err != nil {
		return fmt.Errorf("database %q has an invalid .env: %v", target, err)
	}
	if err := SetActiveDB(target); err != nil {
		return err
	}
	fmt.Printf("\n%s✓ Active database: %s%s%s\n\n", Fmt.Green, Fmt.Bold, target, Fmt.Reset)
	return nil
}

func printSwitchList(names []string, state *State) {
	fmt.Printf("\n%sConfigured databases%s  %s(active: %s)%s\n\n",
		Fmt.Bold, Fmt.Reset,
		Fmt.Dim, defaultStr(state.ActiveDB, "—"), Fmt.Reset)

	// Sort by last-used descending so recent appears first.
	sort.SliceStable(names, func(i, j int) bool {
		ti := state.LastUsed[names[i]]
		tj := state.LastUsed[names[j]]
		return ti > tj
	})

	for i, n := range names {
		marker := " "
		if n == state.ActiveDB {
			marker = "★"
		}
		host := ""
		if db, err := LoadDatabase(n); err == nil {
			host = db.Host()
		}
		last := ""
		if ts, ok := state.LastUsed[n]; ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				last = humanAgo(t)
			}
		}
		if last == "" {
			last = "—"
		}
		fmt.Printf("  %s%s%s  %d. %s%-16s%s  %s%-32s%s  %s%s%s\n",
			Fmt.Yellow, marker, Fmt.Reset,
			i+1,
			Fmt.Bold, n, Fmt.Reset,
			Fmt.Dim, host, Fmt.Reset,
			Fmt.Dim, last, Fmt.Reset)
	}
}

func humanAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

func defaultStr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func strconvAtoi(s string) (int, error) {
	var n int
	var sign = 1
	if strings.HasPrefix(s, "-") {
		sign = -1
		s = s[1:]
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return sign * n, nil
}

func printSwitchHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo switch%s — change the active database

%sUSAGE%s
  %sodoo switch%s              # list + interactive pick (TTY)
  %sodoo switch%s <name>       # direct switch
  %sodoo switch --list%s       # show the list, change nothing

The list is sorted by last-used; the active database is marked with ★.
A "configured database" is any .env file under ~/.odoo/databases/.
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}
