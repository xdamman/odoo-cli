package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/term"
)

// AccountMoveLine is the JSONL / JSON output shape for `odoo account
// <code>`. Wider than the existing JournalLine (which is bank-line-
// specific) because move-lines on non-bank journals don't have the
// statement-line fields, and downstream filters (grep, jq) need
// partner / account / move metadata to be useful.
//
// statementLineId is included so a pipeline like
//
//	odoo account 580700 --jsonl | jq 'select(.amount==484)' | odoo attach MEM/2026/00036
//
// works seamlessly — attach reads statementLineId when uniqueImportId
// isn't on the record.
type AccountMoveLine struct {
	ID              int     `json:"id"`
	MoveID          int     `json:"moveId"`
	MoveName        string  `json:"moveName,omitempty"`
	Date            string  `json:"date,omitempty"`
	AccountID       int     `json:"accountId"`
	AccountCode     string  `json:"accountCode"`
	AccountName     string  `json:"accountName,omitempty"`
	PartnerID       int     `json:"partnerId,omitempty"`
	PartnerName     string  `json:"partnerName,omitempty"`
	JournalID       int     `json:"journalId,omitempty"`
	JournalCode     string  `json:"journalCode,omitempty"`
	Name            string  `json:"name,omitempty"`
	Ref             string  `json:"ref,omitempty"`
	Debit           float64 `json:"debit,omitempty"`
	Credit          float64 `json:"credit,omitempty"`
	Balance         float64 `json:"balance,omitempty"`
	Amount          float64 `json:"amount,omitempty"` // signed (debit - credit), mirrors chb output
	Currency        string  `json:"currency,omitempty"`
	Reconciled      bool    `json:"reconciled,omitempty"`
	ParentState     string  `json:"parentState,omitempty"`
	StatementLineID int     `json:"statementLineId,omitempty"`
}

// ListAccount is `odoo account <code>` — list every
// account.move.line on the given GL account. Default output is a
// human table when stdout is a TTY, auto-switching to JSONL on
// pipes. Force either format with --jsonl (one record per line,
// pipe-friendly) or --json (one pretty-printed array).
//
// Named ListAccount (not Account) so it doesn't collide with the
// existing Account struct in accounts.go — Account is the resolved
// GL-account record returned by ResolveAccount.
func ListAccount(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printAccountHelp()
		return nil
	}
	spec := FirstPositional(args, "--db")
	if spec == "" {
		return fmt.Errorf("usage: odoo account <code|id>")
	}
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)

	forceJSON := HasFlag(args, "--json")
	forceJSONL := HasFlag(args, "--jsonl")
	pipeOutput := forceJSON || forceJSONL || !isStdoutTTY()
	if !pipeOutput {
		PrintActiveDBBanner(db.Name)
	}

	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}
	acc, err := ResolveAccount(db, uid, spec)
	if err != nil {
		return err
	}

	domain := []interface{}{
		[]interface{}{"account_id", "=", acc.ID},
	}
	if HasFlag(args, "--reconciled") {
		domain = append(domain, []interface{}{"reconciled", "=", true})
	}
	if HasFlag(args, "--unreconciled") {
		domain = append(domain, []interface{}{"reconciled", "=", false})
	}
	if state := GetOption(args, "--state"); state != "" {
		// e.g. --state posted, --state draft
		domain = append(domain, []interface{}{"parent_state", "=", state})
	}

	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		domain,
		[]string{"id", "move_id", "date", "account_id",
			"partner_id", "journal_id", "name", "ref",
			"debit", "credit", "balance", "currency_id",
			"reconciled", "parent_state", "statement_line_id"},
		"date asc, id asc",
	)
	if err != nil {
		return fmt.Errorf("read move lines on %s: %v", acc.Code, err)
	}

	lines := make([]AccountMoveLine, 0, len(rows))
	for _, r := range rows {
		debit := Float(r["debit"])
		credit := Float(r["credit"])
		lines = append(lines, AccountMoveLine{
			ID:              Int(r["id"]),
			MoveID:          FieldID(r["move_id"]),
			MoveName:        FieldName(r["move_id"]),
			Date:            Str(r["date"]),
			AccountID:       FieldID(r["account_id"]),
			AccountCode:     acc.Code,
			AccountName:     acc.Name,
			PartnerID:       FieldID(r["partner_id"]),
			PartnerName:     FieldName(r["partner_id"]),
			JournalID:       FieldID(r["journal_id"]),
			JournalCode:     FieldName(r["journal_id"]),
			Name:            Str(r["name"]),
			Ref:             Str(r["ref"]),
			Debit:           debit,
			Credit:          credit,
			Balance:         Float(r["balance"]),
			Amount:          debit - credit,
			Currency:        FieldName(r["currency_id"]),
			Reconciled:      Bool(r["reconciled"]),
			ParentState:     Str(r["parent_state"]),
			StatementLineID: FieldID(r["statement_line_id"]),
		})
	}

	switch {
	case forceJSON:
		return writeAccountJSON(lines)
	case forceJSONL:
		return writeAccountJSONL(lines)
	case !isStdoutTTY():
		return writeAccountJSONL(lines)
	default:
		return writeAccountTable(acc, lines)
	}
}

func writeAccountJSON(lines []AccountMoveLine) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(lines)
}

func writeAccountJSONL(lines []AccountMoveLine) error {
	enc := json.NewEncoder(os.Stdout)
	for _, ln := range lines {
		if err := enc.Encode(ln); err != nil {
			return err
		}
	}
	return nil
}

func writeAccountTable(acc *Account, lines []AccountMoveLine) error {
	fmt.Printf("\n%s%s%s — %s%s%s · %s%d move-line%s%s\n\n",
		Fmt.Cyan, acc.Code, Fmt.Reset,
		Fmt.Bold, acc.Name, Fmt.Reset,
		Fmt.Dim, len(lines), pluralS(len(lines)), Fmt.Reset)
	if len(lines) == 0 {
		fmt.Println()
		return nil
	}

	var totalDebit, totalCredit float64
	headers := []string{"ID", "Date", "Move", "Partner", "Debit", "Credit", "✓"}
	caps := []int{8, 10, 22, 26, 14, 14, 1}
	rows := make([][]string, 0, len(lines))
	for _, l := range lines {
		rec := ""
		if l.Reconciled {
			rec = "✓"
		}
		rows = append(rows, []string{
			strconv.Itoa(l.ID),
			l.Date,
			Truncate(l.MoveName, caps[2]),
			Truncate(l.PartnerName, caps[3]),
			FmtEUR(l.Debit),
			FmtEUR(l.Credit),
			rec,
		})
		totalDebit += l.Debit
		totalCredit += l.Credit
	}
	renderTable(headers, rows, caps, map[int]bool{0: true, 4: true, 5: true})
	fmt.Printf("\n  %ssum debit %s · sum credit %s · balance %s%s\n\n",
		Fmt.Dim, FmtEUR(totalDebit), FmtEUR(totalCredit),
		FmtEURSigned(totalDebit-totalCredit), Fmt.Reset)
	fmt.Printf("  %sNext:%s pipe %sodoo account %s --jsonl%s into %sodoo unreconcile%s · %sodoo assign <code>%s · %sodoo attach <ref>%s\n\n",
		Fmt.Dim, Fmt.Reset,
		Fmt.Cyan, acc.Code, Fmt.Reset,
		Fmt.Cyan, Fmt.Reset,
		Fmt.Cyan, Fmt.Reset,
		Fmt.Cyan, Fmt.Reset)
	return nil
}

// isStdoutTTY reports whether stdout is a real terminal. Used to
// decide whether `odoo account` prints a human table or raw JSONL.
func isStdoutTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func printAccountHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo account <code|id>%s — list every move-line on a GL account

%sUSAGE%s
  %sodoo account 400000%s                  Human table (TTY)
  %sodoo account 400000 --jsonl%s          One JSON record per line (pipe-friendly)
  %sodoo account 400000 --json%s           Single pretty-printed array
  %sodoo account 400000 | grep …%s         Auto-switches to JSONL on pipe

%sFILTERS%s
  %s--reconciled%s          Only reconciled lines
  %s--unreconciled%s        Only unreconciled lines
  %s--state <s>%s           Only lines whose parent move is in state <s> (draft/posted/cancel)

%sPIPES%s
  %sodoo account 400000 --jsonl | odoo unreconcile --yes%s        Unreconcile every line
  %sodoo account 743000 --jsonl | odoo assign 747040 --yes%s      Reassign to a different account
  %sodoo account 580700 --jsonl | jq … | odoo attach <ref> --yes%s  Reconcile statement lines to invoice

Records are account.move.line objects with id / moveId / date / account /
partner / debit / credit / reconciled fields. Bank suspense lines
include statementLineId so attach can resolve them without a Pull.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset, f.Yellow, f.Reset, f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
	)
}
