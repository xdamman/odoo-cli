package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Unreconcile is `odoo journals <id> unreconcile --account <code|id>
// [--yes] [--dry-run]`. Walks every account.move.line on the given
// journal that's currently reconciled on the named account, collects
// the account.partial.reconcile records pairing them up, previews
// the work, and unlinks the partials.
//
// Use case: cleaning up a journal where the operator (or a previous
// import) mis-reconciled lines on a specific GL account (e.g. 400000),
// and the easiest way forward is a clean slate on that account before
// re-matching with `odoo journals <id> reconcile -i`.
func Unreconcile(jid int, args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printUnreconcileHelp()
		return nil
	}
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)
	PrintActiveDBBanner(db.Name)

	accSpec := strings.TrimSpace(GetOption(args, "--account", "-a"))
	if accSpec == "" {
		return fmt.Errorf("missing --account <code|id> (e.g. --account 400000)")
	}
	dryRun := HasFlag(args, "--dry-run")
	assumeYes := HasFlag(args, "--yes", "-y")
	verbose := HasFlag(args, "-v", "--verbose")

	fmt.Printf("\n%s● Authenticating against %s …%s\n", Fmt.Dim, db.URL, Fmt.Reset)
	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}

	acc, err := ResolveAccount(db, uid, accSpec)
	if err != nil {
		return err
	}

	journal, err := fetchJournalShallow(db, uid, jid)
	if err != nil {
		return err
	}

	fmt.Printf("\n%sUnreconcile every reconciled line%s\n", Fmt.Bold, Fmt.Reset)
	fmt.Printf("  journal  %s#%d%s — %s (%s)\n", Fmt.Cyan, journal.ID, Fmt.Reset, journal.Name, journal.Code)
	fmt.Printf("  account  %s%s%s — %s\n", Fmt.Cyan, acc.Code, Fmt.Reset, acc.Name)
	fmt.Println()

	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{
			[]interface{}{"journal_id", "=", jid},
			[]interface{}{"account_id", "=", acc.ID},
			[]interface{}{"reconciled", "=", true},
		},
		[]string{"id", "move_id", "date", "name", "debit", "credit", "balance", "matched_debit_ids", "matched_credit_ids", "partner_id"},
		"date asc, id asc",
	)
	if err != nil {
		return fmt.Errorf("read move lines on journal #%d / %s: %v", jid, acc.Code, err)
	}
	if len(rows) == 0 {
		fmt.Printf("%s● No reconciled lines on journal #%d for account %s — nothing to do.%s\n\n",
			Fmt.Dim, jid, acc.Code, Fmt.Reset)
		return nil
	}

	partials := map[int]bool{}
	var totalDebit, totalCredit float64
	type sampleRow struct {
		LineID, MoveID int
		Date           string
		Name           string
		Amount         float64
	}
	var samples []sampleRow
	for _, r := range rows {
		for _, key := range []string{"matched_debit_ids", "matched_credit_ids"} {
			if arr, ok := r[key].([]interface{}); ok {
				for _, v := range arr {
					if id := Int(v); id > 0 {
						partials[id] = true
					}
				}
			}
		}
		totalDebit += Float(r["debit"])
		totalCredit += Float(r["credit"])
		samples = append(samples, sampleRow{
			LineID: Int(r["id"]),
			MoveID: FieldID(r["move_id"]),
			Date:   Str(r["date"]),
			Name:   Str(r["name"]),
			Amount: Float(r["balance"]),
		})
	}

	fmt.Printf("%s%d reconciled line%s%s — debit %s · credit %s · %s%d partial-reconcile record%s%s to unlink\n",
		Fmt.Bold, len(rows), pluralS(len(rows)), Fmt.Reset,
		FmtEUR(totalDebit), FmtEUR(totalCredit),
		Fmt.Bold, len(partials), pluralS(len(partials)), Fmt.Reset)
	fmt.Println()

	previewLimit := 10
	if verbose {
		previewLimit = len(samples)
	}
	if previewLimit > len(samples) {
		previewLimit = len(samples)
	}
	for i := 0; i < previewLimit; i++ {
		s := samples[i]
		fmt.Printf("  %s·%s line #%d (move #%d) %s · %s · %s\n",
			Fmt.Dim, Fmt.Reset, s.LineID, s.MoveID, s.Date,
			FmtEURSigned(s.Amount), Truncate(s.Name, 50))
	}
	if previewLimit < len(samples) {
		fmt.Printf("  %s… and %d more line%s (pass -v to list every line)%s\n",
			Fmt.Dim, len(samples)-previewLimit, pluralS(len(samples)-previewLimit), Fmt.Reset)
	}
	fmt.Println()

	if len(partials) == 0 {
		fmt.Printf("%s● No partial-reconcile records found — the lines look reconciled but Odoo has no pairing to unlink. Nothing to do.%s\n\n",
			Fmt.Dim, Fmt.Reset)
		return nil
	}

	if dryRun {
		fmt.Printf("%s(dry-run — re-run with --yes to apply.)%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}
	if !assumeYes && isTTY() {
		fmt.Printf("%sUnlink %d partial-reconcile record%s on %s?%s [y/N] ",
			Fmt.Bold, len(partials), pluralS(len(partials)), db.Host(), Fmt.Reset)
		resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		resp = strings.ToLower(strings.TrimSpace(resp))
		if resp != "y" && resp != "yes" {
			fmt.Println("  cancelled.")
			return nil
		}
	} else if !assumeYes {
		return fmt.Errorf("refusing to write on a non-TTY without --yes")
	}

	ids := make([]interface{}, 0, len(partials))
	for id := range partials {
		ids = append(ids, id)
	}
	if _, err := Exec(db.URL, db.DB, uid, db.Password,
		"account.partial.reconcile", "unlink",
		[]interface{}{ids}, nil); err != nil {
		return fmt.Errorf("unlink partials: %v", err)
	}

	fmt.Printf("\n%s✓ Unlinked %d partial-reconcile record%s — %d line%s now open.%s\n",
		Fmt.Green, len(partials), pluralS(len(partials)), len(rows), pluralS(len(rows)), Fmt.Reset)
	fmt.Printf("  Run %sodoo pull%s to refresh the local cache, then %sodoo journals %d reconcile -i%s to re-match.\n\n",
		Fmt.Cyan, Fmt.Reset, Fmt.Cyan, jid, Fmt.Reset)
	return nil
}

// fetchJournalShallow returns a slim journal record for header
// printing. Falls back to a stub when the journal isn't in the cache
// and Odoo doesn't return it (shouldn't happen — but a bad id should
// surface as an error, not a panic).
func fetchJournalShallow(db *Database, uid, jid int) (Journal, error) {
	if f, ok := readJournalsCache(db.Name); ok {
		for _, j := range f.Journals {
			if j.ID == jid {
				return j, nil
			}
		}
	}
	rows, err := SearchReadAllMaps(db, uid, "account.journal",
		[]interface{}{[]interface{}{"id", "=", jid}},
		[]string{"id", "name", "code", "type", "currency_id"}, "")
	if err != nil {
		return Journal{}, fmt.Errorf("fetch journal #%d: %v", jid, err)
	}
	if len(rows) == 0 {
		return Journal{}, fmt.Errorf("journal #%d not found on Odoo", jid)
	}
	r := rows[0]
	return Journal{
		ID:       Int(r["id"]),
		Name:     Str(r["name"]),
		Code:     Str(r["code"]),
		Type:     Str(r["type"]),
		Currency: FieldName(r["currency_id"]),
	}, nil
}

func printUnreconcileHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo journals <id> unreconcile%s — unreconcile every reconciled move-line on a journal+account

%sUSAGE%s
  %sodoo journals 47 unreconcile --account 400000%s         Dry-run preview
  %sodoo journals 47 unreconcile --account 400000 --yes%s   Apply (skips y/N prompt)
  %sodoo journals 47 unreconcile -a 400000 -v%s             Verbose preview

%sBEHAVIOUR%s
  Searches every account.move.line on the given journal that is
  currently reconciled on the named account, collects the
  account.partial.reconcile records linking them, and unlinks those
  partials in a single RPC. The lines themselves are left in place —
  only their reconciliation pairings are removed.

  Use this when a journal's matches on a specific GL account are
  wrong en masse and you want a clean slate to re-run %sodoo journals
  %s<id>%s reconcile -i%s against.

  The account can be given as account code (e.g. "400000") or numeric
  Odoo id.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Reset, f.Reset,
	)
}
