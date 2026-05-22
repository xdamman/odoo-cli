package cmd

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Account is the slim shape we care about for the `accounts` family.
// Code is the canonical operator-facing identifier (e.g. "400000");
// ID is what Odoo's RPC needs.
type Account struct {
	ID   int    `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
	Type string `json:"type"` // account_type
}

// ResolveAccount looks up an account by either numeric Odoo id or
// account code (e.g. "400000"). The search is case-insensitive on the
// code so operators can type either "400000" or "400" (which is then
// only accepted when exactly one match exists).
func ResolveAccount(db *Database, uid int, spec string) (*Account, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("missing account (pass --account <code|id>)")
	}
	// Numeric: try Odoo id first, then fall through to code if no hit.
	if n, err := strconv.Atoi(spec); err == nil {
		// Heuristic: pure-numeric strings might be a code (most Odoo
		// charts of accounts are 6-digit codes) OR a low integer id.
		// Try as code first to match operator intent.
		if acc, ok := findAccountByCode(db, uid, spec); ok {
			return acc, nil
		}
		if acc, ok := findAccountByID(db, uid, n); ok {
			return acc, nil
		}
		return nil, fmt.Errorf("no account found with code or id %q", spec)
	}
	if acc, ok := findAccountByCode(db, uid, spec); ok {
		return acc, nil
	}
	return nil, fmt.Errorf("no account found with code %q", spec)
}

func findAccountByCode(db *Database, uid int, code string) (*Account, bool) {
	rows, err := SearchReadAllMaps(db, uid, "account.account",
		[]interface{}{[]interface{}{"code", "=", code}},
		[]string{"id", "code", "name", "account_type"},
		"id asc",
	)
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	r := rows[0]
	return &Account{
		ID:   Int(r["id"]),
		Code: Str(r["code"]),
		Name: Str(r["name"]),
		Type: Str(r["account_type"]),
	}, true
}

func findAccountByID(db *Database, uid, id int) (*Account, bool) {
	rows, err := SearchReadAllMaps(db, uid, "account.account",
		[]interface{}{[]interface{}{"id", "=", id}},
		[]string{"id", "code", "name", "account_type"},
		"",
	)
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	r := rows[0]
	return &Account{
		ID:   Int(r["id"]),
		Code: Str(r["code"]),
		Name: Str(r["name"]),
		Type: Str(r["account_type"]),
	}, true
}

// Accounts dispatches `odoo accounts …`. Currently only `move`.
func Accounts(args []string) error {
	if HasFlag(args, "--help", "-h", "help") && FirstPositional(args, "--db") == "" {
		printAccountsHelp()
		return nil
	}
	verb := FirstPositional(args, "--db")
	switch verb {
	case "":
		printAccountsHelp()
		return nil
	case "move":
		return accountsMove(args)
	default:
		return fmt.Errorf("unknown accounts verb %q (try: move)", verb)
	}
}

// accountsMove implements `odoo accounts move <from> <to> [--yes] [--dry-run]`.
//
// Walks every account.move.line currently on <from>, groups them by
// move_id, and re-writes account_id to <to>. Posted moves go through
// the canonical draft → write → repost dance via withMoveTemporarilyDraft.
//
// Reconciled lines on the source account are surfaced in the preview
// but NOT touched here — the line's reconciliation pairing depends on
// account equality so a silent account swap would corrupt it. The
// operator is asked to run `odoo journals <id> unreconcile --account
// <from>` first when reconciled lines show up.
func accountsMove(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printAccountsMoveHelp()
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

	pos := positionalsAfter(args, "move", "--db")
	if len(pos) < 2 {
		return fmt.Errorf("usage: odoo accounts move <from-code|id> <to-code|id>")
	}
	fromSpec, toSpec := pos[0], pos[1]

	fmt.Printf("\n%s● Authenticating against %s …%s\n", Fmt.Dim, db.URL, Fmt.Reset)
	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}

	src, err := ResolveAccount(db, uid, fromSpec)
	if err != nil {
		return fmt.Errorf("source account: %v", err)
	}
	dst, err := ResolveAccount(db, uid, toSpec)
	if err != nil {
		return fmt.Errorf("destination account: %v", err)
	}
	if src.ID == dst.ID {
		return fmt.Errorf("source and destination are the same account (%s — %s)", src.Code, src.Name)
	}

	fmt.Printf("\n%sMove every move-line%s\n", Fmt.Bold, Fmt.Reset)
	fmt.Printf("  from  %s%s%s — %s\n", Fmt.Cyan, src.Code, Fmt.Reset, src.Name)
	fmt.Printf("  to    %s%s%s — %s\n", Fmt.Cyan, dst.Code, Fmt.Reset, dst.Name)
	fmt.Println()

	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{[]interface{}{"account_id", "=", src.ID}},
		[]string{"id", "move_id", "date", "name", "debit", "credit", "balance", "reconciled", "journal_id", "partner_id", "parent_state"},
		"date asc, id asc",
	)
	if err != nil {
		return fmt.Errorf("read move lines on %s: %v", src.Code, err)
	}
	if len(rows) == 0 {
		fmt.Printf("%s● No move-lines on account %s — nothing to do.%s\n\n", Fmt.Dim, src.Code, Fmt.Reset)
		return nil
	}

	moveOrder := make([]int, 0)
	moveLines := map[int][]int{}     // moveID → [lineID, …]
	moveState := map[int]string{}    // moveID → parent_state
	movePreview := map[int]string{}  // moveID → date / name
	var totalDebit, totalCredit float64
	reconciledCount := 0
	for _, r := range rows {
		lineID := Int(r["id"])
		moveID := FieldID(r["move_id"])
		if moveID == 0 {
			continue
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
		if Bool(r["reconciled"]) {
			reconciledCount++
		}
	}

	sort.Ints(moveOrder)
	fmt.Printf("%s%d move-line%s%s across %s%d move%s%s · debit %s · credit %s\n",
		Fmt.Bold, len(rows), pluralS(len(rows)), Fmt.Reset,
		Fmt.Bold, len(moveOrder), pluralS(len(moveOrder)), Fmt.Reset,
		FmtEUR(totalDebit), FmtEUR(totalCredit))
	if reconciledCount > 0 {
		fmt.Printf("  %s⚠ %d line%s reconciled — they'll keep their pairings but you almost certainly want to unreconcile first via `odoo journals <id> unreconcile --account %s`%s\n",
			Fmt.Yellow, reconciledCount, pluralS(reconciledCount), src.Code, Fmt.Reset)
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
	if !assumeYes && isTTY() {
		fmt.Printf("%sMove %d line%s from %s to %s on %s?%s [y/N] ",
			Fmt.Bold, len(rows), pluralS(len(rows)), src.Code, dst.Code, db.Host(), Fmt.Reset)
		resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		resp = strings.ToLower(strings.TrimSpace(resp))
		if resp != "y" && resp != "yes" {
			fmt.Println("  cancelled.")
			return nil
		}
	} else if !assumeYes {
		return fmt.Errorf("refusing to write on a non-TTY without --yes")
	}

	var applied, failed int
	var failedMoves []int
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
			// Cancel state shouldn't normally show in account.move.line
			// (Odoo strips lines on cancel) but if it does, write
			// directly — withMoveTemporarilyDraft refuses cancelled.
			err = write()
		default:
			err = withMoveTemporarilyDraft(db, uid, mid, write)
		}
		if err != nil {
			failed++
			failedMoves = append(failedMoves, mid)
			fmt.Printf("  %s✗%s move #%d: %v\n", Fmt.Red, Fmt.Reset, mid, err)
			continue
		}
		applied += len(lineIDs)
		if verbose {
			fmt.Printf("  %s✓%s move #%d — %d line%s\n",
				Fmt.Green, Fmt.Reset, mid, len(lineIDs), pluralS(len(lineIDs)))
		}
	}

	fmt.Printf("\n%sMoved %d line%s%s", Fmt.Green, applied, pluralS(applied), Fmt.Reset)
	if failed > 0 {
		fmt.Printf("  %s(%d move%s failed)%s", Fmt.Red, failed, pluralS(failed), Fmt.Reset)
	}
	fmt.Println()
	fmt.Println()
	return nil
}

// positionalsAfter returns the positional args that come AFTER the
// given verb. Skips value-flags (--db <name>, …) and the verb itself.
func positionalsAfter(args []string, verb string, valueFlags ...string) []string {
	out := make([]string, 0, len(args))
	skip := false
	pastVerb := false
loop:
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		for _, vf := range valueFlags {
			if a == vf {
				skip = true
				continue loop
			}
		}
		if len(a) > 0 && a[0] == '-' {
			continue
		}
		if !pastVerb {
			if a == verb {
				pastVerb = true
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func printAccountsHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo accounts%s — bulk operations on Odoo accounts

%sUSAGE%s
  %sodoo accounts move <from> <to>%s         Move every move-line from <from> to <to>
  %sodoo accounts move <from> <to> --yes%s   Apply without the y/N prompt
  %sodoo accounts move <from> <to> -v%s      Verbose progress

Accounts are resolved by code (e.g. "400000") or numeric Odoo id.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}

func printAccountsMoveHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo accounts move%s — reassign every move-line from one account to another

%sUSAGE%s
  %sodoo accounts move <from> <to>%s         Dry-run preview
  %sodoo accounts move <from> <to> --yes%s   Apply (skips the y/N prompt)
  %sodoo accounts move <from> <to> -v%s      List every affected move

%sBEHAVIOUR%s
  Searches every account.move.line on the source account, groups them
  by their parent move, and rewrites account_id on each line to the
  destination account. Posted moves go through draft → write → repost
  so Odoo accepts the change.

  Reconciled lines are surfaced in the preview but NOT auto-unreconciled
  — run %sodoo journals <id> unreconcile --account <from>%s first if needed.

  Both accounts can be given as account code (e.g. "743000") or numeric
  Odoo id.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
	)
}
