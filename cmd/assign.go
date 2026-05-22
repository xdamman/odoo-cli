package cmd

import (
	"fmt"
	"os"
	"sort"

	"golang.org/x/term"
)

// Assign is `odoo assign <to-code>` — reads JSONL on stdin and
// rewrites each piped move-line's account_id to the named account.
// The bulk form (move every line on account A to account B without
// filtering) is still `odoo accounts move <from> <to>`; this command
// is for the pipe path where the operator has filtered the
// candidates first (typically `odoo account <code> --jsonl | jq …`).
//
// Per-move dance is the same as accounts-move: posted moves go
// through draft → write → repost via withMoveTemporarilyDraft so
// Odoo's "can't edit posted lines" guard doesn't trip.
func Assign(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printAssignHelp()
		return nil
	}
	spec := FirstPositional(args, "--db")
	if spec == "" {
		return fmt.Errorf("usage: odoo assign <to-code|id>  (reads JSONL on stdin)")
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
	if dryRun {
		assumeYes = false
	}

	if term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("expected JSONL on stdin — pipe move-line ids (e.g. `odoo account 743000 --jsonl | odoo assign 747040`)")
	}

	ids, err := readPipedLineIDs(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %v", err)
	}
	if len(ids) == 0 {
		return fmt.Errorf("no move-line ids on stdin")
	}

	fmt.Printf("\n%s● Authenticating against %s …%s\n", Fmt.Dim, db.URL, Fmt.Reset)
	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}
	dst, err := ResolveAccount(db, uid, spec)
	if err != nil {
		return fmt.Errorf("destination account: %v", err)
	}

	// Fetch the parent move + current account for every piped id, in
	// ONE search_read. We need parent_state to know whether the move
	// needs the draft/repost dance, and account_id to skip lines
	// already on the destination.
	idsAny := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		idsAny = append(idsAny, id)
	}
	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{[]interface{}{"id", "in", idsAny}},
		[]string{"id", "move_id", "account_id", "date", "name",
			"debit", "credit", "reconciled", "parent_state"},
		"id asc",
	)
	if err != nil {
		return fmt.Errorf("read move lines: %v", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("none of the %d piped ids match an account.move.line", len(ids))
	}

	moveOrder := make([]int, 0)
	moveLines := map[int][]int{}
	moveState := map[int]string{}
	movePreview := map[int]string{}
	var (
		totalDebit, totalCredit float64
		alreadyOnDest           int
		reconciled              int
	)
	for _, r := range rows {
		lineID := Int(r["id"])
		moveID := FieldID(r["move_id"])
		accID := FieldID(r["account_id"])
		if accID == dst.ID {
			alreadyOnDest++
			continue
		}
		if Bool(r["reconciled"]) {
			reconciled++
		}
		if _, seen := moveLines[moveID]; !seen {
			moveOrder = append(moveOrder, moveID)
		}
		moveLines[moveID] = append(moveLines[moveID], lineID)
		if s := Str(r["parent_state"]); s != "" {
			moveState[moveID] = s
		}
		if _, ok := movePreview[moveID]; !ok {
			movePreview[moveID] = fmt.Sprintf("%s · %s", Str(r["date"]), Truncate(Str(r["name"]), 36))
		}
		totalDebit += Float(r["debit"])
		totalCredit += Float(r["credit"])
	}

	if len(moveLines) == 0 {
		fmt.Printf("%s● Every piped line is already on %s — nothing to do.%s\n\n",
			Fmt.Dim, dst.Code, Fmt.Reset)
		return nil
	}
	sort.Ints(moveOrder)

	totalLines := 0
	for _, ids := range moveLines {
		totalLines += len(ids)
	}
	fmt.Printf("\n%sReassign %d move-line%s%s across %s%d move%s%s → %s%s%s (%s)\n",
		Fmt.Bold, totalLines, pluralS(totalLines), Fmt.Reset,
		Fmt.Bold, len(moveOrder), pluralS(len(moveOrder)), Fmt.Reset,
		Fmt.Cyan, dst.Code, Fmt.Reset, dst.Name)
	fmt.Printf("  %sdebit %s · credit %s%s\n",
		Fmt.Dim, FmtEUR(totalDebit), FmtEUR(totalCredit), Fmt.Reset)
	if alreadyOnDest > 0 {
		fmt.Printf("  %s· %d line%s already on %s — skipped%s\n",
			Fmt.Dim, alreadyOnDest, pluralS(alreadyOnDest), dst.Code, Fmt.Reset)
	}
	if reconciled > 0 {
		fmt.Printf("  %s⚠ %d line%s reconciled — reassigning will leave the pairing dangling. Consider `odoo account <code> --reconciled --jsonl | odoo unreconcile` first.%s\n",
			Fmt.Yellow, reconciled, pluralS(reconciled), Fmt.Reset)
	}
	fmt.Println()

	previewLimit := 10
	if verbose {
		previewLimit = len(moveOrder)
	}
	if previewLimit > len(moveOrder) {
		previewLimit = len(moveOrder)
	}
	for i := 0; i < previewLimit; i++ {
		mid := moveOrder[i]
		state := defaultIfEmpty(moveState[mid], "?")
		fmt.Printf("  %s·%s move #%d (%s) — %s — %d line%s\n",
			Fmt.Dim, Fmt.Reset, mid, state, movePreview[mid],
			len(moveLines[mid]), pluralS(len(moveLines[mid])))
	}
	if previewLimit < len(moveOrder) {
		fmt.Printf("  %s… and %d more move%s (pass -v to list every move)%s\n",
			Fmt.Dim, len(moveOrder)-previewLimit, pluralS(len(moveOrder)-previewLimit), Fmt.Reset)
	}
	fmt.Println()

	if dryRun {
		fmt.Printf("%s(dry-run — re-run with --yes to apply.)%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}
	if !assumeYes {
		ok, terr := confirmTTY(fmt.Sprintf("Reassign %d line%s to %s on %s?",
			totalLines, pluralS(totalLines), dst.Code, db.Host()))
		if terr != nil {
			return fmt.Errorf("no controlling terminal for confirmation (%v) — re-run with --yes", terr)
		}
		if !ok {
			fmt.Println("  cancelled.")
			return nil
		}
	}

	var applied, failed int
	for _, mid := range moveOrder {
		lineIDs := moveLines[mid]
		if len(lineIDs) == 0 {
			continue
		}
		ids := make([]interface{}, 0, len(lineIDs))
		for _, id := range lineIDs {
			ids = append(ids, id)
		}
		write := func() error {
			_, werr := Exec(db.URL, db.DB, uid, db.Password,
				"account.move.line", "write",
				[]interface{}{ids, map[string]interface{}{"account_id": dst.ID}}, nil)
			return werr
		}
		var err error
		switch moveState[mid] {
		case "draft", "cancel":
			err = write()
		default:
			err = withMoveTemporarilyDraft(db, uid, mid, write)
		}
		if err != nil {
			failed++
			fmt.Printf("  %s✗%s move #%d: %v\n", Fmt.Red, Fmt.Reset, mid, err)
			continue
		}
		applied += len(lineIDs)
		if verbose {
			fmt.Printf("  %s✓%s move #%d — %d line%s\n",
				Fmt.Green, Fmt.Reset, mid, len(lineIDs), pluralS(len(lineIDs)))
		}
	}
	fmt.Printf("\n%sReassigned %d line%s to %s%s", Fmt.Green, applied, pluralS(applied), dst.Code, Fmt.Reset)
	if failed > 0 {
		fmt.Printf("  %s(%d move%s failed)%s", Fmt.Red, failed, pluralS(failed), Fmt.Reset)
	}
	fmt.Println()
	fmt.Println()
	return nil
}

func printAssignHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo assign <to-code|id>%s — reassign piped move-lines to a different GL account

%sUSAGE%s
  %sodoo account 743000 --jsonl | odoo assign 747040%s          Dry-run preview
  %sodoo account 743000 --jsonl | odoo assign 747040 --yes%s    Apply
  %s… | jq 'select(.partnerId==42)' | odoo assign 747040 --yes%s  Filter first

%sBEHAVIOUR%s
  Reads JSONL on stdin (one record per line). Only the %sid%s field is
  used — must be an account.move.line id. Lines already on the
  destination account are skipped silently.

  Groups lines by their parent move, then for each move:
    posted   → draft → rewrite account_id → repost
    draft    → rewrite account_id directly
    cancel   → rewrite directly (shouldn't normally appear)

  Reconciled lines emit a warning — Odoo's reconcile pairing is
  account-scoped, so reassigning will leave the partial-reconcile
  records pointing at the OLD account. Unreconcile them first
  (%sodoo account <code> --reconciled --jsonl | odoo unreconcile%s) when
  that matters.

  The non-piped bulk form lives at %sodoo accounts move <from> <to>%s
  — same machinery, but it sweeps every line on the source account.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}
