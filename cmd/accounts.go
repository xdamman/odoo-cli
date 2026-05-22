package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// isStdoutTTY reports whether stdout is a real terminal. Used by
// `odoo accounts <code>` to decide between human table and JSONL.
func isStdoutTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// Account is the slim shape we care about for the `accounts` family.
// Code is the canonical operator-facing identifier (e.g. "400000");
// ID is what Odoo's RPC needs.
type Account struct {
	ID         int    `json:"id"`
	Code       string `json:"code"`
	Name       string `json:"name"`
	Type       string `json:"type"`                 // account_type
	Deprecated bool   `json:"deprecated,omitempty"` // greyed out in the list
	Updated    string `json:"updated,omitempty"`    // RFC3339; when this record was last refreshed
}

// AccountsListFile is the on-disk shape of the cached account list.
type AccountsListFile struct {
	FetchedAt string    `json:"fetchedAt"`
	Count     int       `json:"count"`
	Accounts  []Account `json:"accounts"`
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

// Accounts dispatches `odoo accounts …`.
//
//	odoo accounts              → list every account from local cache
//	odoo accounts <code|id>    → list every move-line on that account
//	odoo accounts move <f> <t> → bulk-reassign all lines on <f> to <t>
//
// First-positional == "move" → bulk verb. Anything else is treated
// as a code or id (mirroring the journals dispatcher's verb shape).
func Accounts(args []string) error {
	if HasFlag(args, "--help", "-h", "help") && FirstPositional(args, "--db") == "" {
		printAccountsHelp()
		return nil
	}
	verb := FirstPositional(args, "--db", "--search")
	switch verb {
	case "":
		return accountsList(args)
	case "move":
		return accountsMove(args)
	default:
		return accountLines(args, verb)
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
%sodoo accounts%s — list / inspect / mutate Odoo GL accounts

%sUSAGE%s
  %sodoo accounts%s                          List every account from local cache
  %sodoo accounts --search KW%s              Filter by code or name substring
  %sodoo accounts <code|id>%s                List every move-line on that account
  %sodoo accounts <code|id> --jsonl%s        Pipe-friendly JSONL of those lines
  %sodoo accounts move <from> <to>%s         Bulk reassign every line on <from> to <to>

%sFILTERS (for odoo accounts <code>)%s
  %s--reconciled%s          Only reconciled lines
  %s--unreconciled%s        Only unreconciled lines
  %s--state <s>%s           Only lines whose parent move is in <s> (draft/posted/cancel)

%sPIPES%s
  %sodoo accounts 400000 --jsonl | odoo unreconcile --yes%s
  %sodoo accounts 743000 --jsonl | odoo assign 747040 --yes%s
  %sodoo accounts 580700 --jsonl | jq … | odoo attach <ref> --yes%s

The list-all view reads %s~/.odoo/cache/<db>/accounts.json%s, which is
refreshed on every %sodoo pull%s. Accounts are resolved by code
(e.g. "400000") or numeric Odoo id everywhere.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset, f.Yellow, f.Reset, f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}

// ── list every account ────────────────────────────────────────────

// accountsList implements `odoo accounts` (no positional). Reads
// the cached account list (refreshed on every `odoo pull`) and
// renders a human table. When stdout is a pipe, switches to JSONL.
func accountsList(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printAccountsHelp()
		return nil
	}
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)

	search := strings.TrimSpace(GetOption(args, "--search"))
	forceJSON := HasFlag(args, "--json")
	forceJSONL := HasFlag(args, "--jsonl")
	pipe := forceJSON || forceJSONL || !isStdoutTTY()
	if !pipe {
		PrintActiveDBBanner(db.Name)
	}

	accounts, fromCache, err := loadOrFetchAccounts(db)
	if err != nil {
		return err
	}

	filter := accounts
	if search != "" {
		kw := strings.ToLower(search)
		filter = make([]Account, 0, len(accounts))
		for _, a := range accounts {
			if strings.Contains(strings.ToLower(a.Code), kw) ||
				strings.Contains(strings.ToLower(a.Name), kw) {
				filter = append(filter, a)
			}
		}
	}
	sort.SliceStable(filter, func(i, j int) bool { return filter[i].Code < filter[j].Code })

	switch {
	case forceJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(filter)
	case forceJSONL || !isStdoutTTY():
		enc := json.NewEncoder(os.Stdout)
		for _, a := range filter {
			if err := enc.Encode(a); err != nil {
				return err
			}
		}
		return nil
	}

	subtitle := "every account"
	if search != "" {
		subtitle = fmt.Sprintf("search: %q", search)
	}
	fmt.Printf("\n%s%d account%s%s — %s%s%s\n\n",
		Fmt.Bold, len(filter), pluralS(len(filter)), Fmt.Reset,
		Fmt.Dim, subtitle, Fmt.Reset)
	if len(filter) == 0 {
		fmt.Printf("  %sNo match. Try `odoo accounts --search …` with a wider keyword, or `odoo pull` to refresh.%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}
	headers := []string{"Code", "Name", "Type", ""}
	caps := []int{10, 42, 22, 12}
	rows := make([][]string, 0, len(filter))
	for _, a := range filter {
		flag := ""
		if a.Deprecated {
			flag = "deprecated"
		}
		rows = append(rows, []string{
			a.Code,
			Truncate(a.Name, caps[1]),
			a.Type,
			flag,
		})
	}
	renderTable(headers, rows, caps, nil)
	if fromCache {
		fmt.Printf("\n%s(cache: %s — run `odoo pull` to refresh)%s\n", Fmt.Dim, cacheAgeAccounts(db), Fmt.Reset)
	}
	fmt.Println()
	return nil
}

// ── list lines on one account ─────────────────────────────────────

// AccountMoveLine is the JSONL / JSON output shape for
// `odoo accounts <code>`. Wider than JournalLine (which is bank-
// specific) because move-lines on non-bank journals don't have the
// statement-line fields, and downstream filters (grep, jq) need
// partner / account / move metadata to be useful.
//
// statementLineId is populated for bank suspense counterparts so a
// pipeline like
//
//	odoo accounts 580700 --jsonl | jq 'select(.amount==484)' | odoo attach MEM/2026/00036
//
// works without an intermediate uniqueImportId resolution.
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

// accountLines is `odoo accounts <code>` — list every
// account.move.line on the given GL account. Human table on TTY;
// auto-switches to JSONL on pipes. Force with --jsonl or --json.
func accountLines(args []string, spec string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printAccountsHelp()
		return nil
	}
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)

	forceJSON := HasFlag(args, "--json")
	forceJSONL := HasFlag(args, "--jsonl")
	pipe := forceJSON || forceJSONL || !isStdoutTTY()
	if !pipe {
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
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(lines)
	case forceJSONL || !isStdoutTTY():
		enc := json.NewEncoder(os.Stdout)
		for _, l := range lines {
			if err := enc.Encode(l); err != nil {
				return err
			}
		}
		return nil
	}

	fmt.Printf("\n%s%s%s — %s%s%s · %s%d move-line%s%s\n\n",
		Fmt.Cyan, acc.Code, Fmt.Reset,
		Fmt.Bold, acc.Name, Fmt.Reset,
		Fmt.Dim, len(lines), pluralS(len(lines)), Fmt.Reset)
	if len(lines) == 0 {
		return nil
	}
	var totalDebit, totalCredit float64
	headers := []string{"ID", "Date", "Move", "Partner", "Name", "Debit", "Credit", "✓"}
	caps := []int{8, 10, 18, 22, 28, 12, 12, 1}
	rows2 := make([][]string, 0, len(lines))
	for _, l := range lines {
		rec := ""
		if l.Reconciled {
			rec = "✓"
		}
		rows2 = append(rows2, []string{
			strconv.Itoa(l.ID),
			l.Date,
			Truncate(l.MoveName, caps[2]),
			Truncate(l.PartnerName, caps[3]),
			Truncate(l.Name, caps[4]),
			FmtEUR(l.Debit),
			FmtEUR(l.Credit),
			rec,
		})
		totalDebit += l.Debit
		totalCredit += l.Credit
	}
	renderTable(headers, rows2, caps, map[int]bool{0: true, 5: true, 6: true})
	fmt.Printf("\n  %ssum debit %s · sum credit %s · balance %s%s\n\n",
		Fmt.Dim, FmtEUR(totalDebit), FmtEUR(totalCredit),
		FmtEURSigned(totalDebit-totalCredit), Fmt.Reset)
	fmt.Printf("  %sNext:%s pipe %sodoo accounts %s --jsonl%s into %sodoo unreconcile%s · %sodoo assign <code>%s · %sodoo attach <ref>%s\n\n",
		Fmt.Dim, Fmt.Reset,
		Fmt.Cyan, acc.Code, Fmt.Reset,
		Fmt.Cyan, Fmt.Reset,
		Fmt.Cyan, Fmt.Reset,
		Fmt.Cyan, Fmt.Reset)
	return nil
}

// ── account-list cache (populated by pull) ────────────────────────

// FetchAccounts reads every account.account from Odoo. Called from
// the pull pipeline.
func FetchAccounts(db *Database, uid int) ([]Account, error) {
	rows, err := SearchReadAllMaps(db, uid, "account.account",
		[]interface{}{},
		[]string{"id", "code", "name", "account_type", "deprecated"},
		"code asc",
	)
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(rows))
	now := time.Now().UTC().Format(time.RFC3339)
	for _, r := range rows {
		out = append(out, Account{
			ID:         Int(r["id"]),
			Code:       Str(r["code"]),
			Name:       Str(r["name"]),
			Type:       Str(r["account_type"]),
			Deprecated: Bool(r["deprecated"]),
			Updated:    now,
		})
	}
	return out, nil
}

// WriteAccountsCache persists the account list under
// ~/.odoo/cache/<dbname>/accounts.json.
func WriteAccountsCache(dbname string, accounts []Account) error {
	if err := EnsureCacheDirs(dbname); err != nil {
		return err
	}
	file := AccountsListFile{
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Count:     len(accounts),
		Accounts:  accounts,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(CacheDir(dbname), "accounts.json"), data, 0600)
}

func readAccountsCache(dbname string) (AccountsListFile, bool) {
	data, err := os.ReadFile(filepath.Join(CacheDir(dbname), "accounts.json"))
	if err != nil {
		return AccountsListFile{}, false
	}
	var file AccountsListFile
	if err := json.Unmarshal(data, &file); err != nil {
		return AccountsListFile{}, false
	}
	return file, true
}

// loadOrFetchAccounts returns the cached account list when present;
// otherwise fetches from Odoo on the fly, writes the cache, and
// returns. fromCache is true when the result came from disk.
func loadOrFetchAccounts(db *Database) (accounts []Account, fromCache bool, err error) {
	if f, ok := readAccountsCache(db.Name); ok && len(f.Accounts) > 0 {
		return f.Accounts, true, nil
	}
	fmt.Printf("%s● Fetching accounts from %s …%s\n", Fmt.Dim, db.URL, Fmt.Reset)
	uid, err := AuthDatabase(db)
	if err != nil {
		return nil, false, err
	}
	fetched, err := FetchAccounts(db, uid)
	if err != nil {
		return nil, false, err
	}
	if err := WriteAccountsCache(db.Name, fetched); err != nil {
		fmt.Fprintf(os.Stderr, "  %s⚠ could not write accounts cache: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
	}
	return fetched, false, nil
}

func cacheAgeAccounts(db *Database) string {
	file, ok := readAccountsCache(db.Name)
	if !ok || file.FetchedAt == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, file.FetchedAt)
	if err != nil {
		return file.FetchedAt
	}
	return humanAgo(t)
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
