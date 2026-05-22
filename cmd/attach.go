package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// AttachTx is the on-the-wire shape of one piped transaction.
// Permissive: EITHER uniqueImportId OR statementLineId is required.
// chb's `transactions` output uses uniqueImportId; `odoo account
// <code> --jsonl` emits statementLineId on suspense lines. Either
// resolves to a single account.bank.statement.line.
//
// Extra fields are ignored — feeding the whole record from upstream
// is fine.
type AttachTx struct {
	UniqueImportID     string  `json:"uniqueImportId,omitempty"`
	StatementLineID    int     `json:"statementLineId,omitempty"`
	DisplayDescription string  `json:"displayDescription,omitempty"`
	Amount             float64 `json:"amount,omitempty"`
}

// Attach is `odoo attach <move-ref>`. Reads JSONL on stdin (one tx
// per line), resolves the target move by name, walks each piped tx
// to its account.bank.statement.line via unique_import_id, and
// batch-reconciles every found bank line with the invoice's A/R/A/P
// line in a single Odoo call.
//
// Typical pipeline:
//
//	chb transactions --amount 484 --since 20260430 | odoo attach MEM/2026/00036 --yes
//
// Stdin must be a pipe (not a TTY). Confirmation flow follows the
// rest of odoo-cli: default is dry-run preview, --yes applies.
// No interactive prompt — stdin is consumed by JSONL.
func Attach(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printAttachHelp()
		return nil
	}

	ref := FirstPositional(args, "--db")
	if ref == "" {
		return fmt.Errorf("usage: odoo attach <move-ref>  (e.g. MEM/2026/00036)")
	}

	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)
	PrintActiveDBBanner(db.Name)

	assumeYes := HasFlag(args, "--yes", "-y")
	dryRun := HasFlag(args, "--dry-run")
	verbose := HasFlag(args, "-v", "--verbose")
	force := HasFlag(args, "--force", "-f")
	if dryRun {
		assumeYes = false
	}

	// Stdin must be a real pipe / file redirect — never an interactive
	// terminal. (Using term.IsTerminal rather than the local isTTY
	// helper so a `< /dev/null` redirect doesn't get mistaken for a
	// TTY — ModeCharDevice is true for null devices too.)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("expected JSONL on stdin — pipe one tx per line (e.g. `chb transactions … | odoo attach %s`)", ref)
	}

	txs, err := readAttachTxs(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %v", err)
	}
	if len(txs) == 0 {
		return fmt.Errorf("no transactions in stdin")
	}

	fmt.Printf("\n%s● Authenticating against %s …%s\n", Fmt.Dim, db.URL, Fmt.Reset)
	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}

	// Resolve the target invoice/bill from its move name.
	invoice, err := findMoveByName(db, uid, ref)
	if err != nil {
		return err
	}

	// Resolve each tx to a statement line. Skip-with-warning on
	// missing uniqueImportId/statementLineId or unknown line.
	// Already-reconciled lines are skipped by default, but --force
	// collects them so we can unreconcile-and-reattach in one pass.
	lines := make([]JournalLine, 0, len(txs))
	forcedLines := make([]JournalLine, 0)
	var skipped int
	for i, tx := range txs {
		desc := FirstNonEmpty(tx.DisplayDescription, tx.UniqueImportID,
			fmt.Sprintf("statementLineId=%d", tx.StatementLineID),
			fmt.Sprintf("tx #%d", i+1))
		var (
			ln    JournalLine
			found bool
			lerr  error
		)
		switch {
		case tx.StatementLineID > 0:
			ln, found, lerr = findStatementLineByID(db, uid, tx.StatementLineID)
		case tx.UniqueImportID != "":
			ln, found, lerr = findStatementLineByUniqueImportID(db, uid, tx.UniqueImportID)
		default:
			fmt.Printf("  %s⚠ %s: needs uniqueImportId or statementLineId — skipped%s\n", Fmt.Yellow, desc, Fmt.Reset)
			skipped++
			continue
		}
		if lerr != nil {
			return fmt.Errorf("lookup %s: %v", desc, lerr)
		}
		if !found {
			fmt.Printf("  %s⚠ %s: no statement line resolved — skipped%s\n", Fmt.Yellow, desc, Fmt.Reset)
			skipped++
			continue
		}
		if ln.IsReconciled {
			if !force {
				fmt.Printf("  %s⚠ %s: bank line #%d already reconciled — skipped (re-run with --force to unreconcile + reattach, or unreconcile first)%s\n",
					Fmt.Yellow, desc, ln.ID, Fmt.Reset)
				skipped++
				continue
			}
			forcedLines = append(forcedLines, ln)
		}
		lines = append(lines, ln)
	}

	if len(lines) == 0 {
		return fmt.Errorf("nothing to attach (%d tx in, %d skipped)", len(txs), skipped)
	}

	// Preview.
	fmt.Printf("\n%sAttach %d statement line%s to%s\n", Fmt.Bold, len(lines), pluralS(len(lines)), Fmt.Reset)
	fmt.Printf("  %s%s%s — %s\n", Fmt.Cyan, invoice.Name, Fmt.Reset, invoice.PartnerName)
	fmt.Printf("  %s%s · residual %s · total %s%s\n",
		Fmt.Dim,
		FirstNonEmpty(invoice.PaymentState, invoice.State),
		FmtAmount(invoice.Residual, invoice.Currency),
		FmtAmount(invoice.Amount, invoice.Currency),
		Fmt.Reset)
	fmt.Println()

	var sumAmount float64
	for _, ln := range lines {
		fmt.Printf("  %s·%s line #%d · %s · %s · %s\n",
			Fmt.Dim, Fmt.Reset, ln.ID, ln.Date,
			FmtEURSigned(ln.Amount),
			Truncate(FirstNonEmpty(ln.PaymentRef, ln.Narration), 50))
		sumAmount += ln.Amount
	}
	fmt.Printf("\n  %ssum: %s · invoice residual: %s%s\n",
		Fmt.Dim, FmtEURSigned(sumAmount),
		FmtAmount(invoice.Residual, invoice.Currency), Fmt.Reset)
	if delta := sumAmount - invoice.Residual; delta < -0.005 || delta > 0.005 {
		fmt.Printf("  %s⚠ sum doesn't match residual (Δ %s) — Odoo will still accept; the invoice will be partially paid.%s\n",
			Fmt.Yellow, FmtEURSigned(delta), Fmt.Reset)
	}
	if skipped > 0 {
		fmt.Printf("  %s%d tx skipped (see warnings above)%s\n", Fmt.Yellow, skipped, Fmt.Reset)
	}
	if len(forcedLines) > 0 {
		fmt.Printf("  %s↻ %d line%s already reconciled — will unreconcile + reattach (--force)%s\n",
			Fmt.Yellow, len(forcedLines), pluralS(len(forcedLines)), Fmt.Reset)
	}
	fmt.Println()

	if dryRun {
		fmt.Printf("%s(dry-run — re-run with --yes to apply.)%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}
	if !assumeYes {
		ok, terr := confirmTTY(fmt.Sprintf("Attach %d line%s to %s on %s?",
			len(lines), pluralS(len(lines)), invoice.Name, db.Host()))
		if terr != nil {
			return fmt.Errorf("no controlling terminal for confirmation (%v) — re-run with --yes", terr)
		}
		if !ok {
			fmt.Println("  cancelled.")
			return nil
		}
	}

	// --force pre-pass: unreconcile each already-reconciled line so
	// AttachLinesToInvoice's draft → rewrite → reconcile dance has
	// clean counterparts to work with.
	for _, fl := range forcedLines {
		n, err := unreconcileForReattach(db, uid, fl)
		if err != nil {
			return fmt.Errorf("force-unreconcile line #%d: %v", fl.ID, err)
		}
		fmt.Printf("  %s↻%s unreconciled %d partial%s on line #%d\n",
			Fmt.Yellow, Fmt.Reset, n, pluralS(n), fl.ID)
	}

	if err := AttachLinesToInvoice(db, uid, lines, *invoice); err != nil {
		return err
	}

	fmt.Printf("\n%s✓ Attached %d line%s to %s on %s%s\n\n",
		Fmt.Green, len(lines), pluralS(len(lines)), invoice.Name, db.Host(), Fmt.Reset)
	if verbose {
		for _, ln := range lines {
			fmt.Printf("  %s✓%s line #%d · %s · %s\n",
				Fmt.Green, Fmt.Reset, ln.ID, ln.Date, FmtEURSigned(ln.Amount))
		}
		fmt.Println()
	}
	return nil
}

// readAttachTxs parses JSONL. One AttachTx per non-blank line; lines
// that don't parse are reported on stderr and skipped, so a single
// malformed entry doesn't kill the whole batch.
func readAttachTxs(r io.Reader) ([]AttachTx, error) {
	scanner := bufio.NewScanner(r)
	// Bump the buffer well above the 64KB default so chb's wider
	// records (descriptions + raw payloads) don't tip us over.
	scanner.Buffer(make([]byte, 1<<20), 8<<20)
	var out []AttachTx
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var tx AttachTx
		if err := json.Unmarshal([]byte(line), &tx); err != nil {
			fmt.Fprintf(os.Stderr, "  %s⚠ line %d: %v — skipped%s\n", Fmt.Yellow, lineNum, err, Fmt.Reset)
			continue
		}
		out = append(out, tx)
	}
	return out, scanner.Err()
}

// findMoveByName looks up an account.move by name (the invoice
// number, e.g. "MEM/2026/00036"). Refuses ambiguous matches so the
// operator gets a clear error instead of attaching to a random pick.
func findMoveByName(db *Database, uid int, name string) (*Invoice, error) {
	rows, err := SearchReadAllMaps(db, uid, "account.move",
		[]interface{}{[]interface{}{"name", "=", name}},
		[]string{"id", "name", "move_type", "state", "payment_state",
			"invoice_date", "date", "amount_total_signed", "amount_residual",
			"currency_id", "partner_id", "ref"},
		"id desc",
	)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no account.move with name %q on Odoo (check the reference)", name)
	}
	if len(rows) > 1 {
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, fmt.Sprintf("#%d", Int(r["id"])))
		}
		return nil, fmt.Errorf("%d moves match name %q (%s) — pass a more specific reference", len(rows), name, strings.Join(ids, ", "))
	}
	r := rows[0]
	return &Invoice{
		ID:           Int(r["id"]),
		Name:         Str(r["name"]),
		MoveType:     Str(r["move_type"]),
		State:        Str(r["state"]),
		PaymentState: Str(r["payment_state"]),
		InvoiceDate:  Str(r["invoice_date"]),
		Date:         Str(r["date"]),
		Amount:       Float(r["amount_total_signed"]),
		Residual:     Float(r["amount_residual"]),
		Currency:     FieldName(r["currency_id"]),
		PartnerID:    FieldID(r["partner_id"]),
		PartnerName:  FieldName(r["partner_id"]),
		Reference:    Str(r["ref"]),
	}, nil
}

// findStatementLineByUniqueImportID looks up one
// account.bank.statement.line by its unique_import_id, returning
// (line, true, nil) when found. unique_import_id is the canonical
// dedup key chb writes when importing transactions, so this is the
// right lookup for piped tx → Odoo line.
func findStatementLineByUniqueImportID(db *Database, uid int, uniqueImportID string) (JournalLine, bool, error) {
	return findStatementLineWhere(db, uid,
		[]interface{}{[]interface{}{"unique_import_id", "=", uniqueImportID}})
}

// findStatementLineByID looks up one account.bank.statement.line by
// its primary id — used when piping from `odoo account <code>`,
// which emits statementLineId on bank suspense lines.
func findStatementLineByID(db *Database, uid, id int) (JournalLine, bool, error) {
	return findStatementLineWhere(db, uid,
		[]interface{}{[]interface{}{"id", "=", id}})
}

// unreconcileForReattach undoes the existing reconciliation on a
// bank statement line so AttachLinesToInvoice can re-attach it to a
// different invoice. Finds the move's non-bank counterpart (the
// line that's currently sitting on the OLD A/R/A/P account), reads
// its matched_debit_ids + matched_credit_ids, and unlinks every
// partial.reconcile pairing it. Returns the number of partials
// removed (0 = the line wasn't actually reconciled).
func unreconcileForReattach(db *Database, uid int, line JournalLine) (int, error) {
	counterpartID, err := findStatementCounterpartLine(db, uid, line)
	if err != nil {
		return 0, fmt.Errorf("find counterpart: %v", err)
	}
	if counterpartID == 0 {
		return 0, fmt.Errorf("could not identify counterpart on move #%d", line.MoveID)
	}
	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{[]interface{}{"id", "=", counterpartID}},
		[]string{"matched_debit_ids", "matched_credit_ids"},
		"",
	)
	if err != nil {
		return 0, fmt.Errorf("read counterpart: %v", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	partials := make([]interface{}, 0)
	for _, key := range []string{"matched_debit_ids", "matched_credit_ids"} {
		if arr, ok := rows[0][key].([]interface{}); ok {
			for _, v := range arr {
				if id := Int(v); id > 0 {
					partials = append(partials, id)
				}
			}
		}
	}
	if len(partials) == 0 {
		return 0, nil
	}
	if _, err := Exec(db.URL, db.DB, uid, db.Password,
		"account.partial.reconcile", "unlink",
		[]interface{}{partials}, nil); err != nil {
		return 0, err
	}
	return len(partials), nil
}

func findStatementLineWhere(db *Database, uid int, domain []interface{}) (JournalLine, bool, error) {
	rows, err := SearchReadAllMaps(db, uid, "account.bank.statement.line",
		domain,
		[]string{"id", "journal_id", "move_id", "date", "amount",
			"payment_ref", "narration", "partner_id", "unique_import_id", "is_reconciled"},
		"id asc",
	)
	if err != nil {
		return JournalLine{}, false, err
	}
	if len(rows) == 0 {
		return JournalLine{}, false, nil
	}
	r := rows[0]
	return JournalLine{
		ID:             Int(r["id"]),
		JournalID:      FieldID(r["journal_id"]),
		MoveID:         FieldID(r["move_id"]),
		Date:           Str(r["date"]),
		Amount:         Float(r["amount"]),
		PaymentRef:     Str(r["payment_ref"]),
		Narration:      Str(r["narration"]),
		PartnerID:      FieldID(r["partner_id"]),
		UniqueImportID: Str(r["unique_import_id"]),
		IsReconciled:   Bool(r["is_reconciled"]),
	}, true, nil
}

func printAttachHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo attach <move-ref>%s — batch-reconcile statement lines onto an invoice/bill from JSONL

%sUSAGE%s
  %scat tx.jsonl | odoo attach MEM/2026/00036%s              Dry-run preview
  %scat tx.jsonl | odoo attach MEM/2026/00036 --yes%s        Apply
  %scat tx.jsonl | odoo attach REF --force --yes%s           Auto-unreconcile already-attached lines
  %schb transactions --amount 484 | odoo attach REF --yes%s  Common chb pipe

%sINPUT%s
  One JSON object per line on stdin. Required: EITHER %suniqueImportId%s
  (chb-style) OR %sstatementLineId%s (emitted by %sodoo account <code>
  --jsonl%s). Optional: displayDescription, amount (preview only).
  Extra fields are ignored — chb and odoo account outputs both work
  as-is, including direct cross-piping.

%sBEHAVIOUR%s
  1. Resolves <move-ref> via account.move.search([("name","=",ref)]).
     Ambiguous matches (multiple moves with the same name) are refused.
  2. For each piped tx, looks up the bank statement line by
     unique_import_id on Odoo (no local cache lookup — works on a fresh
     DB without %sodoo pull%s).
  3. Drafts each bank move → rewrites its suspense counterpart to the
     A/R/A/P account → reposts.
  4. Reconciles every counterpart + the A/R/A/P line in ONE Odoo call.
     N txs that collectively settle the invoice close it atomically.

  Skip-with-warning rules:
    • tx missing uniqueImportId
    • no statement line for the uniqueImportId (re-run %sodoo pull%s if you
      think the line should exist)
    • bank line already reconciled (run %sodoo journals <id> unreconcile%s first)

  Already-paid invoices are handled the same way the TUI does it:
  unreconcile the existing match, then reattach the piped lines.

  %s--force%s extends that to the bank-line side: lines already
  reconciled with a DIFFERENT invoice are unreconciled (their
  account.partial.reconcile pairings are unlinked) and re-attached
  in one pass. Without --force those lines are skipped with a
  warning.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
		f.Bold, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Yellow, f.Reset,
	)
}
