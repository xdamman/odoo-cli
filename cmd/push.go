package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Push walks the pending/ queue for the active DB and applies each
// change via the RPC layer. Default is dry-run; pass --yes to apply.
//
// Each pending change is dispatched by Kind. Successful changes
// move to sent/; failures stamp LastError on the file and leave it
// in pending/ for the next run to retry.
func Push(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printPushHelp()
		return nil
	}
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)
	PrintActiveDBBanner(db.Name)

	dryRun := HasFlag(args, "--dry-run")
	assumeYes := HasFlag(args, "--yes", "-y")
	verbose := HasFlag(args, "-v", "--verbose")

	pending, err := ListPending(db.Name)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Printf("\n%sNothing to push — pending queue is empty.%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}

	fmt.Printf("\n%s%d pending change%s queued for push%s\n\n",
		Fmt.Bold, len(pending), pluralS(len(pending)), Fmt.Reset)

	// Preview each change before applying.
	for _, c := range pending {
		printPendingPreview(c, verbose)
	}

	if dryRun {
		fmt.Printf("\n%s(dry-run — re-run with --yes to apply.)%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}

	if !assumeYes && isTTY() {
		fmt.Printf("\n%sApply %d change%s to Odoo at %s?%s [Y/n] ",
			Fmt.Bold, len(pending), pluralS(len(pending)), db.Host(), Fmt.Reset)
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		resp := strings.ToLower(strings.TrimSpace(line))
		if resp == "n" || resp == "no" {
			fmt.Println("  cancelled.")
			return nil
		}
	} else if !assumeYes {
		return fmt.Errorf("refusing to push on a non-TTY without --yes")
	}

	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}

	var applied, failed int
	for _, c := range pending {
		if err := applyPending(db, uid, c); err != nil {
			failed++
			fmt.Printf("  %s✗%s %s %s: %v\n", Fmt.Red, Fmt.Reset, c.Kind, c.ID, err)
			_ = StampPendingError(db.Name, c.ID, err)
			continue
		}
		applied++
		if verbose {
			fmt.Printf("  %s✓%s %s %s\n", Fmt.Green, Fmt.Reset, c.Kind, c.ID)
		}
		if err := MovePendingToSent(db.Name, c.ID); err != nil {
			fmt.Fprintf(os.Stderr, "  %s⚠ archive %s: %v%s\n", Fmt.Yellow, c.ID, err, Fmt.Reset)
		}
	}

	// Stamp the lastSync push timestamp regardless of failures so the
	// dashboard reflects the most recent attempt.
	if last := LoadLastSync(db.Name); last != nil {
		last.PushedAt = time.Now().UTC().Format(time.RFC3339)
		if err := writeLastSync(db.Name, last); err != nil {
			fmt.Fprintf(os.Stderr, "  %s⚠ write _last_sync: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
		}
	}

	fmt.Printf("\n%sPushed %d%s", Fmt.Green, applied, Fmt.Reset)
	if failed > 0 {
		fmt.Printf("  %sFailed %d (will retry next push)%s", Fmt.Red, failed, Fmt.Reset)
	}
	fmt.Println()
	if failed > 0 && !verbose {
		fmt.Printf("%s(re-run with --verbose to see failure details, or inspect ~/.odoo/cache/%s/pending/<id>.json)%s\n",
			Fmt.Dim, db.Name, Fmt.Reset)
	}
	fmt.Println()
	return nil
}

// applyPending dispatches a single change by Kind. New change types
// register their apply functions here.
func applyPending(db *Database, uid int, c PendingChange) error {
	switch c.Kind {
	case "reconcile":
		var p ReconcilePayload
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			return fmt.Errorf("malformed reconcile payload: %w", err)
		}
		return applyReconcilePending(db, uid, p)
	default:
		return fmt.Errorf("unsupported pending kind %q", c.Kind)
	}
}

// applyReconcilePending routes queued reconcile changes through
// the canonical apply path used by the TUI's `-i` flow. The cached
// bank line + invoice are looked up by id; the actual Odoo writes
// (draft → rewrite suspense counterpart → repost → reconcile) live
// in ReconcileBankLineWithInvoice.
func applyReconcilePending(db *Database, uid int, p ReconcilePayload) error {
	return applyReconcilePendingFromCache(db, uid, p)
}

func printPendingPreview(c PendingChange, verbose bool) {
	fmt.Printf("  %s○%s %s%s%s %s%s%s",
		Fmt.Dim, Fmt.Reset,
		Fmt.Bold, c.Kind, Fmt.Reset,
		Fmt.Dim, c.ID, Fmt.Reset)
	if c.Attempts > 0 {
		fmt.Printf("  %s(attempts: %d)%s", Fmt.Yellow, c.Attempts, Fmt.Reset)
	}
	fmt.Println()
	if c.LastError != "" {
		fmt.Printf("    %slast error: %s%s\n", Fmt.Red, c.LastError, Fmt.Reset)
	}
	if verbose {
		fmt.Printf("    %spayload: %s%s\n", Fmt.Dim, summarizePayload(c.Payload), Fmt.Reset)
	}
}

// summarizePayload returns a one-line preview of the payload JSON
// so the verbose dry-run doesn't dump the full body.
func summarizePayload(raw json.RawMessage) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 120 {
		s = s[:117] + "…"
	}
	return s
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func printPushHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo push%s — apply pending local changes to Odoo

%sUSAGE%s
  %sodoo push%s              Dry-run preview
  %sodoo push --yes%s        Apply (skips the y/N prompt)
  %sodoo push -v%s           Per-change details

%sBEHAVIOUR%s
  Reads JSON files from ~/.odoo/cache/<dbname>/pending/ and replays
  each via the RPC layer. Successful changes move to sent/.
  Failures stamp a lastError field on the file and leave it in
  pending/ for the next run to retry.

  Each pending change has a kind ("reconcile", future: "categorize",
  etc.) that dispatches to the right apply function.

%sON DISK%s
  ~/.odoo/cache/<dbname>/pending/  One JSON per queued change.
  ~/.odoo/cache/<dbname>/sent/     Archive of pushed changes.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
	)
}
