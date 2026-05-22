package cmd

import "fmt"

// Sync = Pull + Push. Same flags as Push (--yes / --dry-run / -v).
// Stops at the first failure: a pull error blocks the push so the
// operator doesn't push against a half-pulled cache.
func Sync(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printSyncHelp()
		return nil
	}
	if err := Pull(args); err != nil {
		return err
	}
	fmt.Printf("%s──────── push ────────%s\n", Fmt.Dim, Fmt.Reset)
	return Push(args)
}

func printSyncHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo sync%s — pull + push in one shot

%sUSAGE%s
  %sodoo sync%s              Dry-run preview of the push (pull always runs)
  %sodoo sync --yes%s        Apply changes after the pull
  %sodoo sync -v%s           Verbose progress on both legs

Equivalent to:
  %sodoo pull%s && %sodoo push%s

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset,
	)
}
