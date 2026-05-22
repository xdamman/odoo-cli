package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"
)

// Assign is `odoo assign <to-code>` — reads JSONL on stdin and
// books each piped record onto the named GL account. Two input
// shapes are accepted; whichever fields are present decide the
// path:
//
//	{"id": 12345}                       — Odoo move-line id. Rewrites
//	                                      its account_id (the existing
//	                                      "re-assign" pipeline from
//	                                      `odoo accounts <code> --jsonl`).
//	{"uniqueImportId": "stripe:txn_…"}  — chb-style bank tx. Resolves
//	                                      to the underlying
//	                                      account.bank.statement.line,
//	                                      then to its suspense-side
//	                                      counterpart move-line, and
//	                                      retargets THAT line — i.e.
//	                                      first-time categorisation
//	                                      from chb's --unreconciled
//	                                      filter.
//
// Records may carry both fields; `id` wins when present. Mixed
// stdin (some manual ids, some chb txs) works.
//
// The bulk form (move every line on account A to account B without
// filtering) is still `odoo accounts move <from> <to>`.
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
	force := HasFlag(args, "--force")
	if dryRun {
		assumeYes = false
	}

	if term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("expected JSONL on stdin — pipe Odoo move-line ids (`odoo accounts 743000 --jsonl | odoo assign 740040`) or chb txs (`chb transactions --unreconciled | odoo assign 740040`)")
	}

	records, err := readPipedAssignRecords(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %v", err)
	}

	var directIDs []int
	var importIDs []string
	importContext := map[string]string{} // uiid → "amount EUR · description" for nicer warnings
	for _, r := range records {
		switch {
		case r.ID > 0:
			directIDs = append(directIDs, r.ID)
		case r.UniqueImportID != "":
			importIDs = append(importIDs, r.UniqueImportID)
			importContext[r.UniqueImportID] = r.contextLabel()
		}
	}
	if len(directIDs) == 0 && len(importIDs) == 0 {
		return fmt.Errorf("no usable records on stdin — need `id` (int, Odoo move-line) or `uniqueImportId` (chb bank tx)")
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

	// Resolve chb-style uniqueImportIds to the categorised-side
	// move-line on each statement's move. Returns BOTH still-open
	// and already-reconciled statements so we can show the operator
	// the current account either way.
	var resolvedIDs []int
	if len(importIDs) > 0 {
		fmt.Printf("%s● Resolving %d chb tx%s → categorised move-lines …%s\n",
			Fmt.Dim, len(importIDs), pluralS(len(importIDs)), Fmt.Reset)
		resolutions, unresolved, rerr := resolveImportIDsToCategorisedLines(db, uid, importIDs)
		if rerr != nil {
			return rerr
		}

		// Account-id → "code (name)" labels for the "currently on …"
		// detail line on each skip warning.
		acctIDs := make([]int, 0, len(resolutions))
		for _, r := range resolutions {
			acctIDs = append(acctIDs, r.CurrentAccountID)
		}
		acctLabels := lookupAccountLabels(db, uid, acctIDs)

		var safe, skippedReconciled, forced []chbResolution
		for _, r := range resolutions {
			switch {
			case r.WasReconciled && !force:
				skippedReconciled = append(skippedReconciled, r)
			case r.WasReconciled && force:
				forced = append(forced, r)
				safe = append(safe, r)
				resolvedIDs = append(resolvedIDs, r.MoveLineID)
			default:
				safe = append(safe, r)
				resolvedIDs = append(resolvedIDs, r.MoveLineID)
			}
		}

		fmt.Printf("  %sresolved: %d · unresolved: %d · skipped (already reconciled): %d%s\n",
			Fmt.Dim, len(safe), len(unresolved), len(skippedReconciled), Fmt.Reset)
		printSkippedImportIDs("not found in Odoo (push or pull first?)", unresolved, importContext)
		printSkippedReconciled(skippedReconciled, importContext, acctLabels)
		printForcedResolutions(forced, importContext, acctLabels)
	}

	ids := dedupeInts(append(directIDs, resolvedIDs...))
	if len(ids) == 0 {
		return fmt.Errorf("nothing to assign after resolving stdin")
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
		fmt.Printf("  %s⚠ %d line%s reconciled — reassigning will leave the pairing dangling. Consider `odoo accounts <code> --reconciled --jsonl | odoo unreconcile` first.%s\n",
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
%sodoo assign <to-code|id>%s — book piped lines/txs onto a GL account

%sUSAGE%s
  %s# Re-assign existing Odoo move-lines (the "move from A to B" pipeline)%s
  %sodoo accounts 743000 --jsonl | odoo assign 740040%s          Dry-run preview
  %sodoo accounts 743000 --jsonl | odoo assign 740040 --yes%s    Apply
  %s… | jq 'select(.partnerId==42)' | odoo assign 740040 --yes%s  Filter first

  %s# First-time categorise unreconciled bank txs (the chb pipeline)%s
  %schb transactions --search donation --unreconciled | odoo assign 740040%s
  %schb transactions --since 20260101 --unreconciled | odoo assign 740040 --yes%s

  %s# Force-retarget chb txs whose bank line was already categorised%s
  %schb transactions --search Mohammed | odoo assign 740040 --force%s

%sBEHAVIOUR%s
  Reads JSONL on stdin. Two record shapes accepted:

    %s{"id": 12345}%s                  Odoo account.move.line id. Used
                                   as-is (re-assign existing line).
    %s{"uniqueImportId": "stripe:txn_…"}%s  chb-style bank tx. Resolves via
                                   account.bank.statement.line to the
                                   non-bank-side move-line on that
                                   statement's move (the categorised
                                   side — suspense for an unreconciled
                                   statement, the actual income/expense
                                   account once it's been categorised).
                                   THAT line is retargeted to <to-code>.

  Records with both fields favour %sid%s. Mixed stdin works. Records
  with neither (or with a chb-style %sid%s that's a string, not an int)
  fall back to %suniqueImportId%s.

  By default chb txs whose bank statement is already reconciled in
  Odoo are skipped, with a warning that shows the account they're
  currently booked to. Pass %s--force%s to retarget them anyway — useful
  when a previous categorisation went to the wrong account.

  For each resolved line, groups by parent move, then per move:
    posted   → draft → rewrite account_id → repost
    draft    → rewrite account_id directly
    cancel   → rewrite directly (shouldn't normally appear)

  Reconciled lines emit a warning — Odoo's reconcile pairing is
  account-scoped, so reassigning will leave the partial-reconcile
  records pointing at the OLD account. Unreconcile them first
  (%sodoo accounts <code> --reconciled --jsonl | odoo unreconcile%s) when
  that matters.

  The non-piped bulk form lives at %sodoo accounts move <from> <to>%s
  — same machinery, but it sweeps every line on the source account.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Dim, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
		f.Dim, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset,
		f.Dim, f.Reset, // # Force-retarget chb txs whose bank line was already categorised
		f.Cyan, f.Reset, // chb transactions --search Mohammed … --force
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Cyan, f.Reset, // --force in the BEHAVIOUR sentence
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}

// pipedAssignRecord is the stdin record shape `odoo assign` reads.
// Two source pipelines feed it:
//
//   - `odoo accounts <code> --jsonl` emits {"id": 12345, …} where id
//     is an account.move.line id (int).
//   - `chb transactions --json` emits {"id": "stripe:txn_…", "uniqueImportId":
//     "stripe:acct_…:txn_…", …} where id is chb's namespaced string and
//     uniqueImportId is the bridge to account.bank.statement.line.
//
// IDRaw captures whichever form `id` came in as without forcing a
// type up front; ID gets the int when it parses, otherwise 0 and
// the resolver falls back to UniqueImportID.
type pipedAssignRecord struct {
	IDRaw              json.RawMessage `json:"id"`
	UniqueImportID     string          `json:"uniqueImportId,omitempty"`
	DisplayDescription string          `json:"displayDescription,omitempty"`
	Amount             float64         `json:"amount,omitempty"`
	Currency           string          `json:"currency,omitempty"`

	// Derived in the reader from IDRaw.
	ID int `json:"-"`
}

// contextLabel returns a one-line "amount CCY · description" string
// used in skip warnings so the operator can spot which chb records
// got dropped without scrolling through opaque ids.
func (r pipedAssignRecord) contextLabel() string {
	parts := make([]string, 0, 2)
	if r.Amount != 0 {
		ccy := r.Currency
		if ccy == "" {
			ccy = "EUR"
		}
		parts = append(parts, FmtAmount(r.Amount, ccy))
	}
	if r.DisplayDescription != "" {
		parts = append(parts, CollapseWhitespace(r.DisplayDescription))
	}
	return strings.Join(parts, " · ")
}

// readPipedAssignRecords parses JSONL on r. Blank lines skipped;
// malformed records warned + skipped on stderr. IDs that came as
// strings (chb's namespaced form) are silently demoted to 0 so the
// reader falls back to uniqueImportId rather than warning twice.
func readPipedAssignRecords(r io.Reader) ([]pipedAssignRecord, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)
	var out []pipedAssignRecord
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec pipedAssignRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			fmt.Fprintf(os.Stderr, "  %s⚠ line %d: %v — skipped%s\n", Fmt.Yellow, lineNum, err, Fmt.Reset)
			continue
		}
		// Try to coerce IDRaw → int. Anything non-numeric (chb's
		// "stripe:txn_…") stays 0 and the resolver will fall back to
		// uniqueImportId.
		if len(rec.IDRaw) > 0 && rec.IDRaw[0] != '"' && rec.IDRaw[0] != 'n' {
			_ = json.Unmarshal(rec.IDRaw, &rec.ID)
		}
		if rec.ID == 0 && rec.UniqueImportID == "" {
			fmt.Fprintf(os.Stderr, "  %s⚠ line %d: no `id` (int) or `uniqueImportId` — skipped%s\n", Fmt.Yellow, lineNum, Fmt.Reset)
			continue
		}
		out = append(out, rec)
	}
	return out, scanner.Err()
}

// chbResolution is one resolved chb tx → Odoo move-line link. The
// "categorised side" is the non-bank line on the statement's move:
// for an unreconciled statement that's the suspense line, for an
// already-reconciled one it's whatever real account the operator
// previously booked it onto.
type chbResolution struct {
	UniqueImportID   string // the chb-side identifier (for context)
	MoveLineID       int    // account.move.line to retarget
	CurrentAccountID int    // where the line lives right now
	WasReconciled    bool   // statement line is_reconciled when we looked
}

// resolveImportIDsToCategorisedLines walks the chb pipeline:
//
//	uniqueImportId → account.bank.statement.line → its journal's
//	default_account_id (the bank GL) → every NON-bank line on the
//	statement's move (the categorised side; for an unreconciled
//	statement that's the suspense line, for a reconciled one it's
//	the account the operator already booked it onto).
//
// Per chb tx the resolver may return more than one resolution
// (split categorisations). Both the open and already-reconciled
// statement lines are returned; the caller decides what to do based
// on WasReconciled and the --force flag.
//
// `unresolved` lists chb ids that had no matching account.bank.
// statement.line at all — likely not yet pushed to Odoo.
//
// Three RPCs total (statement lines, journals, move-lines), all
// paginated by SearchReadAllMaps.
func resolveImportIDsToCategorisedLines(db *Database, uid int, importIDs []string) (resolved []chbResolution, unresolved []string, err error) {
	uidsAny := make([]interface{}, 0, len(importIDs))
	for _, u := range importIDs {
		uidsAny = append(uidsAny, u)
	}
	slines, err := SearchReadAllMaps(db, uid, "account.bank.statement.line",
		[]interface{}{[]interface{}{"unique_import_id", "in", uidsAny}},
		[]string{"id", "unique_import_id", "move_id", "journal_id", "is_reconciled"},
		"id asc",
	)
	if err != nil {
		return nil, nil, fmt.Errorf("lookup bank statement lines: %v", err)
	}

	// move_id → (journal_id, is_reconciled, unique_import_id). Built
	// for every statement that came back, regardless of reconciled
	// state — we want to surface the current account either way.
	type moveCtx struct {
		JournalID       int
		WasReconciled   bool
		UniqueImportID  string
	}
	moveCtxByID := map[int]moveCtx{}
	seen := map[string]bool{}
	for _, s := range slines {
		uiid := Str(s["unique_import_id"])
		seen[uiid] = true
		moveCtxByID[FieldID(s["move_id"])] = moveCtx{
			JournalID:      FieldID(s["journal_id"]),
			WasReconciled:  Bool(s["is_reconciled"]),
			UniqueImportID: uiid,
		}
	}
	for _, u := range importIDs {
		if !seen[u] {
			unresolved = append(unresolved, u)
		}
	}
	if len(moveCtxByID) == 0 {
		return nil, unresolved, nil
	}

	// Look up each journal's bank GL (default_account_id). The
	// "non-bank" filter below uses it to pick the categorised side
	// regardless of whether that side is the suspense account or a
	// real income/expense account.
	journalIDSet := map[int]struct{}{}
	for _, ctx := range moveCtxByID {
		if ctx.JournalID > 0 {
			journalIDSet[ctx.JournalID] = struct{}{}
		}
	}
	jidsAny := make([]interface{}, 0, len(journalIDSet))
	for jid := range journalIDSet {
		jidsAny = append(jidsAny, jid)
	}
	journals, err := SearchReadAllMaps(db, uid, "account.journal",
		[]interface{}{[]interface{}{"id", "in", jidsAny}},
		[]string{"id", "default_account_id"},
		"id asc",
	)
	if err != nil {
		return nil, unresolved, fmt.Errorf("lookup journals: %v", err)
	}
	bankByJournal := map[int]int{}
	for _, j := range journals {
		bankByJournal[Int(j["id"])] = FieldID(j["default_account_id"])
	}

	// Every line on every candidate move; pick the non-bank-side
	// ones. A single move can produce >1 resolution when the
	// operator split the statement across multiple accounts.
	movesAny := make([]interface{}, 0, len(moveCtxByID))
	for mid := range moveCtxByID {
		movesAny = append(movesAny, mid)
	}
	mlines, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{[]interface{}{"move_id", "in", movesAny}},
		[]string{"id", "move_id", "account_id"},
		"id asc",
	)
	if err != nil {
		return nil, unresolved, fmt.Errorf("lookup move-lines: %v", err)
	}
	for _, m := range mlines {
		mid := FieldID(m["move_id"])
		accID := FieldID(m["account_id"])
		ctx := moveCtxByID[mid]
		if bank := bankByJournal[ctx.JournalID]; bank > 0 && accID == bank {
			continue // skip the bank-side line
		}
		resolved = append(resolved, chbResolution{
			UniqueImportID:   ctx.UniqueImportID,
			MoveLineID:       Int(m["id"]),
			CurrentAccountID: accID,
			WasReconciled:    ctx.WasReconciled,
		})
	}
	return resolved, unresolved, nil
}

// lookupAccountLabels batch-reads account.account names for the
// given ids. Used to render "currently on 743000 (Miscellaneous
// operating income)" in skip warnings. Empty / errored → empty map;
// callers degrade gracefully (just show the id).
func lookupAccountLabels(db *Database, uid int, ids []int) map[int]string {
	out := map[int]string{}
	if len(ids) == 0 {
		return out
	}
	any := make([]interface{}, 0, len(ids))
	seen := map[int]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		any = append(any, id)
	}
	rows, err := SearchReadAllMaps(db, uid, "account.account",
		[]interface{}{[]interface{}{"id", "in", any}},
		[]string{"id", "code", "name"},
		"code asc",
	)
	if err != nil {
		return out
	}
	for _, r := range rows {
		code := Str(r["code"])
		name := Str(r["name"])
		if code == "" && name == "" {
			continue
		}
		if code == "" {
			out[Int(r["id"])] = name
		} else {
			out[Int(r["id"])] = fmt.Sprintf("%s (%s)", code, name)
		}
	}
	return out
}

// printSkippedImportIDs prints a capped list of uniqueImportIds with
// their chb-side context (amount + description) so the operator can
// see which records didn't make it through resolution.
func printSkippedImportIDs(reason string, ids []string, context map[string]string) {
	if len(ids) == 0 {
		return
	}
	limit := 5
	if limit > len(ids) {
		limit = len(ids)
	}
	fmt.Printf("  %s⚠ %d %s%s\n", Fmt.Yellow, len(ids), reason, Fmt.Reset)
	for i := 0; i < limit; i++ {
		ctx := context[ids[i]]
		if ctx == "" {
			fmt.Printf("    %s· %s%s\n", Fmt.Dim, ids[i], Fmt.Reset)
		} else {
			fmt.Printf("    %s· %s — %s%s\n", Fmt.Dim, ctx, ids[i], Fmt.Reset)
		}
	}
	if limit < len(ids) {
		fmt.Printf("    %s… and %d more%s\n", Fmt.Dim, len(ids)-limit, Fmt.Reset)
	}
}

// printSkippedReconciled formats the "already reconciled — currently
// on X" warning for chb txs the resolver mapped to an already-
// reconciled statement (no --force). Each row shows the chb context
// and the account the line is currently booked to, so the operator
// can see exactly what they'd be overriding.
func printSkippedReconciled(rs []chbResolution, context map[string]string, acctLabels map[int]string) {
	if len(rs) == 0 {
		return
	}
	limit := 5
	if limit > len(rs) {
		limit = len(rs)
	}
	fmt.Printf("  %s⚠ %d already reconciled — pass %s--force%s%s to retarget anyway%s\n",
		Fmt.Yellow, len(rs), Fmt.Cyan, Fmt.Reset+Fmt.Yellow, "", Fmt.Reset)
	for i := 0; i < limit; i++ {
		r := rs[i]
		ctx := context[r.UniqueImportID]
		acct := defaultIfEmpty(acctLabels[r.CurrentAccountID], fmt.Sprintf("account #%d", r.CurrentAccountID))
		switch {
		case ctx != "":
			fmt.Printf("    %s· %s → currently on %s%s\n", Fmt.Dim, ctx, acct, Fmt.Reset)
		default:
			fmt.Printf("    %s· %s → currently on %s%s\n", Fmt.Dim, r.UniqueImportID, acct, Fmt.Reset)
		}
	}
	if limit < len(rs) {
		fmt.Printf("    %s… and %d more%s\n", Fmt.Dim, len(rs)-limit, Fmt.Reset)
	}
}

// printForcedResolutions notes which already-reconciled chb txs got
// pulled into the write set because --force was passed. Same shape
// as printSkippedReconciled but framed as an action, not a skip.
func printForcedResolutions(rs []chbResolution, context map[string]string, acctLabels map[int]string) {
	if len(rs) == 0 {
		return
	}
	limit := 5
	if limit > len(rs) {
		limit = len(rs)
	}
	fmt.Printf("  %s↻ %d already-reconciled line%s pulled in by --force%s\n",
		Fmt.Cyan, len(rs), pluralS(len(rs)), Fmt.Reset)
	for i := 0; i < limit; i++ {
		r := rs[i]
		ctx := context[r.UniqueImportID]
		acct := defaultIfEmpty(acctLabels[r.CurrentAccountID], fmt.Sprintf("account #%d", r.CurrentAccountID))
		if ctx != "" {
			fmt.Printf("    %s· %s → moving from %s%s\n", Fmt.Dim, ctx, acct, Fmt.Reset)
		} else {
			fmt.Printf("    %s· %s → moving from %s%s\n", Fmt.Dim, r.UniqueImportID, acct, Fmt.Reset)
		}
	}
	if limit < len(rs) {
		fmt.Printf("    %s… and %d more%s\n", Fmt.Dim, len(rs)-limit, Fmt.Reset)
	}
}

// dedupeInts preserves first-seen order. Used after merging directIDs
// and resolvedIDs so a record showing up in both buckets (rare but
// possible if the operator pipes both stdin sources via xargs/cat)
// doesn't double-write.
func dedupeInts(in []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
