package cmd

import "fmt"

// PrintTopHelp prints the top-level usage when invoked without a
// command or with `odoo --help`.
func PrintTopHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo%s %s — local-first CLI for everyday Odoo operations

%sUSAGE%s
  %sodoo%s [%s--db <name>%s] <command> [args]

%sCOMMANDS%s
  %ssetup%s                       Add a new database (interactive walkthrough)
  %sswitch%s [<dbname>]           Change the active database
  %sjournals%s [--search KW] [--all]
                              List journals (favorites by default)
  %sjournals%s <id>               Show details for a journal
  %sjournals%s <id> favorite      Mark a journal as favorite
  %sjournals%s <id> unfavorite    Remove from favorites
  %sjournals%s <id> reconcile [-i] [--yes]
                              Reconcile unmatched bank lines on a journal
  %sjournals%s <id> unreconcile --account <code|id> [--yes]
                              Unlink every reconciliation on a journal+account
  %saccounts%s [--search KW]      List every GL account from the cache
  %saccounts%s <code|id>          List move-lines on an account (JSONL when piped)
  %saccounts%s move <from> <to> [--yes]
                              Bulk reassign every line on <from> to <to>
  %sreconcile%s --account <code> [-i] [--yes]
                              Pair debits/credits on an account (same-account reconcile)
  %sattach%s <invoice-ref> [--yes]
                              Pipe JSONL → batch-reconcile lines onto an invoice
  %sassign%s <to-code> [--yes]    Pipe JSONL → reassign lines to a different account
  %sunreconcile%s [--yes]         Pipe JSONL → unlink reconciliation pairings
  %spull%s                        Refresh local cache from Odoo (favorites + invoices + bills)
  %spush%s [--yes]                Apply pending local changes to Odoo
  %ssync%s [--yes]                pull + push
  %supdate%s [--check] [--yes]    Self-update from GitHub releases

%sGLOBAL FLAGS%s
  %s--db%s <name>                 Override the active database for this invocation
  %s-h%s, %s--help%s                 Show contextual help (works on any sub-command)
  %s-V%s, %s--version%s              Show version
  %s-v%s, %s--verbose%s              Per-row detail (sub-commands only)

%sCONVENTIONS%s
  • Non-interactive by default; pair %s-i%s with any sub-command that supports a TUI.
  • Destructive operations (push, reconcile apply) require %s--yes%s or a TTY prompt.
  • Active database is shown at the top of every output. Run %sodoo switch%s to change.

%sON DISK%s
  ~/.odoo/databases/<dbname>.env     One env file per Odoo database (ODOO_URL=…)
  ~/.odoo/state.json                  Active database + recent-use state
  ~/.odoo/cache/<dbname>/             Local cache (journals, invoices, bills, partners)
  ~/.odoo/cache/<dbname>/pending/     Pending changes waiting to be pushed
  ~/.odoo/cache/<dbname>/sent/        Archive of pushed changes
  ~/.odoo/keys/                        Reserved for signing material (SSH-style)

%sHELP%s
  %sodoo <command> --help%s        Per-command details

`,
		f.Bold, f.Reset, Version,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset, // update
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset, f.Yellow, f.Reset,
		f.Yellow, f.Reset, f.Yellow, f.Reset,
		f.Yellow, f.Reset, f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
	)
}

// PrintActiveDBBanner is called at the top of every command's
// output. Sub-commands that don't need a DB (setup, switch, help)
// skip it.
func PrintActiveDBBanner(name string) {
	if name == "" {
		fmt.Printf("%s(no database configured — run `odoo setup`)%s\n", Fmt.Dim, Fmt.Reset)
		return
	}
	fmt.Printf("%sdb:%s %s%s%s\n", Fmt.Dim, Fmt.Reset, Fmt.Bold, name, Fmt.Reset)
}
