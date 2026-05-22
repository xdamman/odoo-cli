package cmd

import (
	"encoding/json"
	"fmt"
)

// ReconcileBankLineWithInvoice is odoo-cli's port of chb's
// reconcileStatementLineWithMove. Walks the canonical Odoo
// reconcile dance:
//
//  1. Find the invoice's open A/R or A/P line. If none (already
//     fully paid), try the payment's outstanding-receipts line;
//     failing that, unreconcile every existing match on the
//     invoice's A/R line so we can reattach the bank counterpart
//     to a freshly-reopened line.
//  2. Find the bank statement line's non-bank counterpart
//     (currently on the suspense account). Refuse to touch the
//     bank-side line itself — that would trip Odoo's "exactly one
//     entry on the journal's default account" invariant.
//  3. Draft the bank move → rewrite the suspense counterpart's
//     account_id to the invoice's A/R account → repost.
//  4. Reconcile the (now matching-account) counterpart + invoice
//     A/R lines.
//  5. Best-effort: attribute the bank line to the invoice's
//     partner_id so Odoo's reconcile widget proposes the right
//     candidates next time.
//
// On success the local journal-lines cache is patched
// (IsReconciled=true on the bank line) so the next `odoo journals
// <id> reconcile` doesn't surface the same pair again.
func ReconcileBankLineWithInvoice(db *Database, uid int, line JournalLine, invoice Invoice) error {
	if line.MoveID == 0 {
		return fmt.Errorf("statement line #%d has no move id", line.ID)
	}

	// 1. Locate the open A/R or A/P line on the invoice.
	arLineID, arAccountID, arErr := findOpenReceivablePayableLine(db, uid, invoice.ID)
	if arErr != nil {
		// Fallback: try unreconciling existing matches on the
		// invoice's A/R line and reusing the freshly-reopened line.
		if relineID, reaccID, unreconciled, uerr := unreconcileInvoiceAR(db, uid, invoice.ID); uerr == nil && relineID > 0 {
			fmt.Printf("  %s↻ unreconciled %d existing match%s on %s #%d before re-attaching%s\n",
				Fmt.Yellow, unreconciled, pluralS(unreconciled), invoice.MoveType, invoice.ID, Fmt.Reset)
			arLineID = relineID
			arAccountID = reaccID
		} else {
			return fmt.Errorf("invoice #%d: %v", invoice.ID, arErr)
		}
	}

	// 2. Find the bank move's suspense counterpart line.
	counterpartID, err := findStatementCounterpartLine(db, uid, line)
	if err != nil {
		return fmt.Errorf("find counterpart on move #%d: %v", line.MoveID, err)
	}
	if counterpartID == 0 {
		return fmt.Errorf("could not identify the suspense counterpart on move #%d", line.MoveID)
	}

	// 3. Draft → rewrite counterpart account → repost.
	if err := withMoveTemporarilyDraft(db, uid, line.MoveID, func() error {
		_, werr := Exec(db.URL, db.DB, uid, db.Password,
			"account.move.line", "write",
			[]interface{}{[]interface{}{counterpartID}, map[string]interface{}{"account_id": arAccountID}}, nil)
		if werr != nil {
			return fmt.Errorf("rewrite counterpart line #%d: %v", counterpartID, werr)
		}
		return nil
	}); err != nil {
		return err
	}

	// 4. Reconcile.
	if _, err := Exec(db.URL, db.DB, uid, db.Password,
		"account.move.line", "reconcile",
		[]interface{}{[]interface{}{counterpartID, arLineID}}, nil); err != nil {
		return fmt.Errorf("reconcile lines #%d ↔ #%d: %v", counterpartID, arLineID, err)
	}

	// 5. Best-effort partner attribution on the bank line.
	if invoice.PartnerID > 0 && line.PartnerID == 0 {
		_, _ = Exec(db.URL, db.DB, uid, db.Password,
			"account.bank.statement.line", "write",
			[]interface{}{[]interface{}{line.ID}, map[string]interface{}{"partner_id": invoice.PartnerID}}, nil)
	}

	// Patch the local journal-lines cache so this pair doesn't
	// resurface next run.
	if f := loadJournalLines(db.Name, line.JournalID); f != nil {
		patched := false
		for i := range f.Lines {
			if f.Lines[i].ID == line.ID {
				f.Lines[i].IsReconciled = true
				patched = true
				break
			}
		}
		if patched {
			_ = WriteJournalLines(db.Name, line.JournalID, f.Lines)
		}
	}
	return nil
}

// findOpenReceivablePayableLine returns the (lineID, accountID) of
// the invoice's still-unreconciled A/R or A/P line. Filtered by
// account_type (more reliable than the per-account reconcile flag,
// which can be set on revenue / VAT accounts on some installs).
func findOpenReceivablePayableLine(db *Database, uid int, moveID int) (int, int, error) {
	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{
			[]interface{}{"move_id", "=", moveID},
			[]interface{}{"account_type", "in", []interface{}{"asset_receivable", "liability_payable"}},
			[]interface{}{"reconciled", "=", false},
		},
		[]string{"id", "account_id", "amount_residual"},
		"id asc",
	)
	if err != nil {
		return 0, 0, err
	}
	if len(rows) == 0 {
		return 0, 0, fmt.Errorf("no open A/R or A/P line on move #%d (likely already paid)", moveID)
	}
	// Pick the line with the largest |residual| — handles partially-
	// reconciled moves where multiple A/R lines exist.
	bestIdx := 0
	bestResidual := abs(Float(rows[0]["amount_residual"]))
	for i := 1; i < len(rows); i++ {
		r := abs(Float(rows[i]["amount_residual"]))
		if r > bestResidual {
			bestResidual = r
			bestIdx = i
		}
	}
	lineID := Int(rows[bestIdx]["id"])
	accountID := FieldID(rows[bestIdx]["account_id"])
	if accountID == 0 {
		return 0, 0, fmt.Errorf("A/R line #%d on move #%d has no account", lineID, moveID)
	}
	return lineID, accountID, nil
}

// unreconcileInvoiceAR undoes every existing reconciliation on the
// invoice/bill's A/R or A/P lines and returns the (now-open) primary
// line + its account. Used when the operator picks an
// already-reconciled invoice from the picker, signalling that the
// previous reconciliation was wrong.
//
// Returns (lineID, accountID, partialsRemoved, error). lineID = 0
// when the move has no A/R line at all.
func unreconcileInvoiceAR(db *Database, uid int, invoiceMoveID int) (int, int, int, error) {
	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{
			[]interface{}{"move_id", "=", invoiceMoveID},
			[]interface{}{"account_type", "in", []interface{}{"asset_receivable", "liability_payable"}},
		},
		[]string{"id", "account_id", "matched_debit_ids", "matched_credit_ids"},
		"id asc",
	)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(rows) == 0 {
		return 0, 0, 0, nil
	}
	partials := map[int]bool{}
	var lineIDs []int
	for _, r := range rows {
		if id := Int(r["id"]); id > 0 {
			lineIDs = append(lineIDs, id)
		}
		for _, key := range []string{"matched_debit_ids", "matched_credit_ids"} {
			if arr, ok := r[key].([]interface{}); ok {
				for _, v := range arr {
					if id := Int(v); id > 0 {
						partials[id] = true
					}
				}
			}
		}
	}
	if len(partials) > 0 {
		ids := make([]interface{}, 0, len(partials))
		for id := range partials {
			ids = append(ids, id)
		}
		if _, err := Exec(db.URL, db.DB, uid, db.Password,
			"account.partial.reconcile", "unlink",
			[]interface{}{ids}, nil); err != nil {
			return 0, 0, 0, fmt.Errorf("unreconcile partials: %v", err)
		}
	}
	// Re-read the primary A/R line so the caller gets a fresh
	// account_id + the residual we just reopened.
	primaryID, primaryAcc, ferr := findOpenReceivablePayableLine(db, uid, invoiceMoveID)
	if ferr != nil {
		// All lines might still be reported as "reconciled" if Odoo
		// hasn't recomputed yet — fall back to whatever we have.
		if len(lineIDs) > 0 {
			read, _ := SearchReadAllMaps(db, uid, "account.move.line",
				[]interface{}{[]interface{}{"id", "=", lineIDs[0]}},
				[]string{"id", "account_id"}, "")
			if len(read) > 0 {
				return lineIDs[0], FieldID(read[0]["account_id"]), len(partials), nil
			}
		}
		return 0, 0, len(partials), nil
	}
	return primaryID, primaryAcc, len(partials), nil
}

// findStatementCounterpartLine returns the move.line on the bank
// move that's currently on the suspense account (i.e. the non-bank
// side of the move). NEVER returns the bank line — Odoo's
// "exactly one line on the journal's default account" rule would
// refuse the write that follows.
func findStatementCounterpartLine(db *Database, uid int, line JournalLine) (int, error) {
	defaultAccountID, err := fetchJournalDefaultAccount(db, uid, line.MoveID)
	if err != nil {
		return 0, fmt.Errorf("resolve journal default account: %v", err)
	}
	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{[]interface{}{"move_id", "=", line.MoveID}},
		[]string{"id", "balance", "debit", "credit", "reconciled", "account_id"},
		"id asc",
	)
	if err != nil {
		return 0, err
	}
	var unreconciled, all []int
	for _, r := range rows {
		id := Int(r["id"])
		accID := FieldID(r["account_id"])
		if defaultAccountID > 0 && accID == defaultAccountID {
			continue
		}
		balance := Float(r["balance"])
		debit := Float(r["debit"])
		credit := Float(r["credit"])
		matchesSign := (line.Amount > 0 && (balance < -0.005 || credit > 0.005)) ||
			(line.Amount < 0 && (balance > 0.005 || debit > 0.005))
		if !matchesSign {
			continue
		}
		all = append(all, id)
		if !Bool(r["reconciled"]) {
			unreconciled = append(unreconciled, id)
		}
	}
	if len(unreconciled) == 1 {
		return unreconciled[0], nil
	}
	if len(all) == 1 {
		return all[0], nil
	}
	return 0, nil
}

// fetchJournalDefaultAccount returns the journal.default_account_id
// of the journal that owns the given move. Used by the counterpart
// lookup to skip the bank-side line.
func fetchJournalDefaultAccount(db *Database, uid, moveID int) (int, error) {
	if moveID <= 0 {
		return 0, nil
	}
	moveRows, err := SearchReadAllMaps(db, uid, "account.move",
		[]interface{}{[]interface{}{"id", "=", moveID}},
		[]string{"id", "journal_id"}, "")
	if err != nil {
		return 0, err
	}
	if len(moveRows) == 0 {
		return 0, nil
	}
	journalID := FieldID(moveRows[0]["journal_id"])
	if journalID == 0 {
		return 0, nil
	}
	jrnRows, err := SearchReadAllMaps(db, uid, "account.journal",
		[]interface{}{[]interface{}{"id", "=", journalID}},
		[]string{"id", "default_account_id"}, "")
	if err != nil {
		return 0, err
	}
	if len(jrnRows) == 0 {
		return 0, nil
	}
	return FieldID(jrnRows[0]["default_account_id"]), nil
}

// withMoveTemporarilyDraft is the "draft → mutate → repost" helper.
// Reads the current state up front so we don't try button_draft on
// an already-draft move (Odoo rejects) and don't auto-post a move
// the operator intentionally left in draft.
//
// The mutate step runs inside a deferred-style block: if fn fails,
// we still repost the move (when it was posted to begin with) so
// nothing gets stranded in draft.
func withMoveTemporarilyDraft(db *Database, uid, moveID int, fn func() error) error {
	if moveID == 0 {
		return fmt.Errorf("missing move id")
	}
	state, err := readMoveState(db, uid, moveID)
	if err != nil {
		return fmt.Errorf("read move #%d state: %v", moveID, err)
	}
	switch state {
	case "draft":
		return fn()
	case "posted":
		if _, derr := Exec(db.URL, db.DB, uid, db.Password,
			"account.move", "button_draft",
			[]interface{}{[]interface{}{moveID}}, nil); derr != nil {
			return fmt.Errorf("draft move #%d: %v", moveID, derr)
		}
		fnErr := fn()
		_, postErr := Exec(db.URL, db.DB, uid, db.Password,
			"account.move", "action_post",
			[]interface{}{[]interface{}{moveID}}, nil)
		if fnErr != nil {
			return fnErr
		}
		if postErr != nil {
			return fmt.Errorf("repost move #%d: %v", moveID, postErr)
		}
		return nil
	case "cancel":
		return fmt.Errorf("move #%d is cancelled; refusing to mutate", moveID)
	default:
		return fmt.Errorf("move #%d has unexpected state %q", moveID, state)
	}
}

// readMoveState returns the `state` field of an account.move.
func readMoveState(db *Database, uid, moveID int) (string, error) {
	rows, err := SearchReadAllMaps(db, uid, "account.move",
		[]interface{}{[]interface{}{"id", "=", moveID}},
		[]string{"id", "state"}, "")
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return Str(rows[0]["state"]), nil
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// ── Pending payload ↔ apply bridge ───────────────────────────────

// applyReconcilePendingFromCache implements push's "reconcile"
// apply path. Resolves the cached bank line + invoice by id, then
// calls ReconcileBankLineWithInvoice.
func applyReconcilePendingFromCache(db *Database, uid int, p ReconcilePayload) error {
	if p.StatementLineID == 0 || p.InvoiceMoveID == 0 {
		return fmt.Errorf("reconcile payload missing ids (line=%d move=%d)", p.StatementLineID, p.InvoiceMoveID)
	}
	// Find the line in the cached journal file.
	f := loadJournalLines(db.Name, p.JournalID)
	if f == nil {
		return fmt.Errorf("journal #%d not in local cache — run `odoo pull`", p.JournalID)
	}
	var line *JournalLine
	for i := range f.Lines {
		if f.Lines[i].ID == p.StatementLineID {
			line = &f.Lines[i]
			break
		}
	}
	if line == nil {
		return fmt.Errorf("statement line #%d not in journal-#%d cache", p.StatementLineID, p.JournalID)
	}
	// Find the invoice in either invoices.json or bills.json.
	var invoice *Invoice
	for _, name := range []string{"invoices.json", "bills.json"} {
		if f, ok := loadCachedInvoices(db.Name, name); ok {
			for i := range f.Invoices {
				if f.Invoices[i].ID == p.InvoiceMoveID {
					invoice = &f.Invoices[i]
					break
				}
			}
		}
		if invoice != nil {
			break
		}
	}
	if invoice == nil {
		return fmt.Errorf("invoice/bill #%d not in local cache — run `odoo pull`", p.InvoiceMoveID)
	}
	return ReconcileBankLineWithInvoice(db, uid, *line, *invoice)
}

// MarshalPendingPayload is a tiny helper so callers don't need to
// import encoding/json just to wrap a payload.
func MarshalPendingPayload(v interface{}) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return data, nil
}
