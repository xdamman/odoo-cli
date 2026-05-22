package cmd

import "fmt"

// Stubs for sub-commands not yet implemented. Each prints a
// "not yet implemented" line and returns nil so the dispatch path
// compiles. Real implementations replace these one by one.
//
// As soon as a real Setup / Switch / Journals / Pull / Push / Sync
// lands in its own file, remove the corresponding stub here.

func Journals(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		fmt.Println("odoo journals [--search KW] [--all] — list journals (not yet implemented)")
		return nil
	}
	return fmt.Errorf("`odoo journals` not yet implemented")
}

func Pull(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		fmt.Println("odoo pull — refresh cache from Odoo (not yet implemented)")
		return nil
	}
	return fmt.Errorf("`odoo pull` not yet implemented")
}

func Push(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		fmt.Println("odoo push [--yes] — apply pending changes (not yet implemented)")
		return nil
	}
	return fmt.Errorf("`odoo push` not yet implemented")
}

func Sync(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		fmt.Println("odoo sync [--yes] — pull + push (not yet implemented)")
		return nil
	}
	return fmt.Errorf("`odoo sync` not yet implemented")
}
