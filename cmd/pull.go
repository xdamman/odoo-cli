package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// JournalLine is the cached shape of one bank-statement line. Narrow
// enough that the reconcile matcher has everything it needs without
// re-fetching, wide enough to render the picker.
type JournalLine struct {
	ID             int     `json:"id"`
	JournalID      int     `json:"journalId"`
	MoveID         int     `json:"moveId"`
	Date           string  `json:"date"`
	Amount         float64 `json:"amount"`
	PaymentRef     string  `json:"paymentRef,omitempty"`
	Narration      string  `json:"narration,omitempty"`
	PartnerID      int     `json:"partnerId,omitempty"`
	UniqueImportID string  `json:"uniqueImportId,omitempty"`
	IsReconciled   bool    `json:"isReconciled,omitempty"`
}

// JournalLinesFile is the on-disk shape per journal.
type JournalLinesFile struct {
	JournalID int           `json:"journalId"`
	FetchedAt string        `json:"fetchedAt"`
	Count     int           `json:"count"`
	Lines     []JournalLine `json:"lines"`
}

// Invoice is the cached shape of a posted invoice/bill, slim enough
// to power the reconcile suggester.
type Invoice struct {
	ID            int     `json:"id"`
	Name          string  `json:"name"`
	MoveType      string  `json:"moveType"`     // out_invoice / in_invoice / out_refund / in_refund
	State         string  `json:"state"`        // draft / posted / cancel
	PaymentState  string  `json:"paymentState"` // not_paid / in_payment / partial / paid / reversed
	InvoiceDate   string  `json:"invoiceDate"`
	Date          string  `json:"date"`
	Amount        float64 `json:"amount"`        // total signed
	Residual      float64 `json:"residual"`      // amount_residual
	Currency      string  `json:"currency"`
	PartnerID     int     `json:"partnerId"`
	PartnerName   string  `json:"partnerName"`
	Reference     string  `json:"reference,omitempty"`
	FirstLineItem string  `json:"firstLineItem,omitempty"`
}

// InvoicesFile is the on-disk shape under cache/<db>/invoices.json
// (and bills.json — same shape, different MoveType filter).
type InvoicesFile struct {
	FetchedAt string    `json:"fetchedAt"`
	Count     int       `json:"count"`
	Invoices  []Invoice `json:"invoices"`
}

// Partner is the index entry for one res.partner.
type Partner struct {
	ID    int      `json:"id"`
	Name  string   `json:"name"`
	IBANs []string `json:"ibans,omitempty"`
}

// PartnersFile is the on-disk partner index.
type PartnersFile struct {
	FetchedAt string             `json:"fetchedAt"`
	Count     int                `json:"count"`
	ByID      map[int]*Partner   `json:"byId"`
	ByIBAN    map[string]int     `json:"byIban,omitempty"`
}

// LastSyncFile tracks per-DB sync timestamps so the operator (and
// the `sync` summary line) knows how fresh each cached subset is.
type LastSyncFile struct {
	PulledAt        string `json:"pulledAt,omitempty"`
	PushedAt        string `json:"pushedAt,omitempty"`
	JournalsCount   int    `json:"journalsCount,omitempty"`
	InvoicesCount   int    `json:"invoicesCount,omitempty"`
	BillsCount      int    `json:"billsCount,omitempty"`
	PartnersCount   int    `json:"partnersCount,omitempty"`
	FavoritesCount  int    `json:"favoritesCount,omitempty"`
}

// Pull refreshes the cache for the active database.
//
// Steps (in order, each best-effort with a per-step warning on failure):
//
//  1. Journals list (account.journal) — populates the favorites
//     resolver and serves `odoo journals --all`.
//  2. For each FAVORITE journal: account.bank.statement.line for
//     bank/cash; account.move.line for everything else. Cached
//     under ~/.odoo/cache/<db>/journals/<id>.json.
//  3. Open invoices (out_invoice + out_refund, posted, not fully paid).
//  4. Open bills (in_invoice + in_refund, posted, not fully paid).
//  5. Partner index (res.partner + res.partner.bank for IBAN map).
//
//  Updates _last_sync.json with counts so `odoo sync` / `odoo journals`
//  can show "cache: <age>".
func Pull(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printPullHelp()
		return nil
	}
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)
	PrintActiveDBBanner(db.Name)

	verbose := HasFlag(args, "-v", "--verbose")
	includeAll := HasFlag(args, "--all") // pull lines for ALL journals, not just favorites

	fmt.Printf("\n%s● Authenticating against %s …%s\n", Fmt.Dim, db.URL, Fmt.Reset)
	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}

	started := time.Now()
	sync := &LastSyncFile{}

	// 1. Journals list
	fmt.Printf("%s● Pulling journals list …%s\n", Fmt.Dim, Fmt.Reset)
	journals, err := FetchJournals(db, uid)
	if err != nil {
		return fmt.Errorf("journals: %w", err)
	}
	if err := WriteJournalsCache(db.Name, journals); err != nil {
		fmt.Fprintf(os.Stderr, "  %s⚠ write journals cache: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
	}
	sync.JournalsCount = len(journals)
	fmt.Printf("  %s✓ %d journals%s\n", Fmt.Green, len(journals), Fmt.Reset)

	// 2. Favorite journal lines
	fav, _ := LoadFavorites(db.Name)
	pullSet := map[int]bool{}
	if includeAll {
		for _, j := range journals {
			pullSet[j.ID] = true
		}
	} else {
		for _, id := range fav.Journals {
			pullSet[id] = true
		}
	}
	sync.FavoritesCount = len(fav.Journals)
	if len(pullSet) == 0 {
		fmt.Printf("  %s↳ no favorite journals — skip lines (run `odoo journals <id> favorite`)%s\n", Fmt.Dim, Fmt.Reset)
	} else {
		for _, j := range journals {
			if !pullSet[j.ID] {
				continue
			}
			fmt.Printf("%s● Pulling lines for journal #%d %s …%s\n", Fmt.Dim, j.ID, j.Name, Fmt.Reset)
			lines, err := FetchJournalLines(db, uid, j)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  %s⚠ journal #%d: %v%s\n", Fmt.Yellow, j.ID, err, Fmt.Reset)
				continue
			}
			if err := WriteJournalLines(db.Name, j.ID, lines); err != nil {
				fmt.Fprintf(os.Stderr, "  %s⚠ write journal #%d: %v%s\n", Fmt.Yellow, j.ID, err, Fmt.Reset)
				continue
			}
			fmt.Printf("  %s✓ %d lines%s\n", Fmt.Green, len(lines), Fmt.Reset)
			if verbose {
				openCount := 0
				for _, l := range lines {
					if !l.IsReconciled {
						openCount++
					}
				}
				fmt.Printf("    %s%d unreconciled · %d reconciled%s\n", Fmt.Dim, openCount, len(lines)-openCount, Fmt.Reset)
			}
		}
	}

	// 3. Open invoices
	fmt.Printf("%s● Pulling open invoices …%s\n", Fmt.Dim, Fmt.Reset)
	invoices, err := FetchInvoices(db, uid, "out_invoice", "out_refund")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s⚠ invoices: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
	} else {
		if err := writeInvoicesFile(db.Name, "invoices.json", invoices); err != nil {
			fmt.Fprintf(os.Stderr, "  %s⚠ write invoices: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
		}
		sync.InvoicesCount = len(invoices)
		fmt.Printf("  %s✓ %d invoices%s\n", Fmt.Green, len(invoices), Fmt.Reset)
	}

	// 4. Open bills
	fmt.Printf("%s● Pulling open bills …%s\n", Fmt.Dim, Fmt.Reset)
	bills, err := FetchInvoices(db, uid, "in_invoice", "in_refund")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s⚠ bills: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
	} else {
		if err := writeInvoicesFile(db.Name, "bills.json", bills); err != nil {
			fmt.Fprintf(os.Stderr, "  %s⚠ write bills: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
		}
		sync.BillsCount = len(bills)
		fmt.Printf("  %s✓ %d bills%s\n", Fmt.Green, len(bills), Fmt.Reset)
	}

	// 5. Partner index
	fmt.Printf("%s● Pulling partner index …%s\n", Fmt.Dim, Fmt.Reset)
	partners, err := FetchPartners(db, uid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s⚠ partners: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
	} else {
		if err := WritePartners(db.Name, partners); err != nil {
			fmt.Fprintf(os.Stderr, "  %s⚠ write partners: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
		}
		sync.PartnersCount = len(partners.ByID)
		fmt.Printf("  %s✓ %d partners%s\n", Fmt.Green, len(partners.ByID), Fmt.Reset)
	}

	sync.PulledAt = time.Now().UTC().Format(time.RFC3339)
	if err := writeLastSync(db.Name, sync); err != nil {
		fmt.Fprintf(os.Stderr, "  %s⚠ write _last_sync: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
	}

	fmt.Printf("\n%s✓ Pulled in %s%s\n", Fmt.Green, time.Since(started).Round(100*time.Millisecond), Fmt.Reset)
	fmt.Printf("  Next: %sodoo journals%s · %sodoo journals <id> reconcile -i%s · %sodoo push --yes%s\n\n",
		Fmt.Cyan, Fmt.Reset, Fmt.Cyan, Fmt.Reset, Fmt.Cyan, Fmt.Reset)
	return nil
}

// ── fetchers ────────────────────────────────────────────────────

// FetchJournalLines reads every bank statement line on a journal.
// For non-bank/cash journals, returns nil (the account.move.line
// model has a different shape — not needed for reconcile flows).
func FetchJournalLines(db *Database, uid int, j Journal) ([]JournalLine, error) {
	if j.Type != "bank" && j.Type != "cash" {
		// Not the reconcile target; skip silently.
		return nil, nil
	}
	rows, err := SearchReadAllMaps(db, uid, "account.bank.statement.line",
		[]interface{}{[]interface{}{"journal_id", "=", j.ID}},
		[]string{"id", "journal_id", "move_id", "date", "amount",
			"payment_ref", "narration", "partner_id", "unique_import_id", "is_reconciled"},
		"date asc, id asc",
	)
	if err != nil {
		return nil, err
	}
	out := make([]JournalLine, 0, len(rows))
	for _, r := range rows {
		out = append(out, JournalLine{
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
		})
	}
	return out, nil
}

// WriteJournalLines persists a journal's lines.
func WriteJournalLines(dbname string, jid int, lines []JournalLine) error {
	if err := EnsureCacheDirs(dbname); err != nil {
		return err
	}
	file := JournalLinesFile{
		JournalID: jid,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Count:     len(lines),
		Lines:     lines,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(JournalsCacheDir(dbname), fmt.Sprintf("%d.json", jid))
	return os.WriteFile(path, data, 0600)
}

// FetchInvoices pulls posted, not-fully-paid account.move records
// of the given move types. Used for both invoices (out_invoice +
// out_refund) and bills (in_invoice + in_refund).
func FetchInvoices(db *Database, uid int, moveTypes ...string) ([]Invoice, error) {
	if len(moveTypes) == 0 {
		return nil, fmt.Errorf("no move types specified")
	}
	moveTypeArr := make([]interface{}, 0, len(moveTypes))
	for _, mt := range moveTypes {
		moveTypeArr = append(moveTypeArr, mt)
	}
	rows, err := SearchReadAllMaps(db, uid, "account.move",
		[]interface{}{
			[]interface{}{"state", "=", "posted"},
			[]interface{}{"move_type", "in", moveTypeArr},
			[]interface{}{"payment_state", "not in", []interface{}{"paid", "reversed"}},
		},
		[]string{"id", "name", "move_type", "state", "payment_state",
			"invoice_date", "date", "amount_total_signed", "amount_residual",
			"currency_id", "partner_id", "ref"},
		"invoice_date desc, id desc",
	)
	if err != nil {
		return nil, err
	}
	out := make([]Invoice, 0, len(rows))
	for _, r := range rows {
		out = append(out, Invoice{
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
		})
	}
	return out, nil
}

// writeInvoicesFile persists either invoices.json or bills.json.
func writeInvoicesFile(dbname, name string, items []Invoice) error {
	if err := EnsureCacheDirs(dbname); err != nil {
		return err
	}
	file := InvoicesFile{
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Count:     len(items),
		Invoices:  items,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(CacheDir(dbname), name), data, 0600)
}

// FetchPartners builds the partner index. Pulls every res.partner
// (lightweight: just id + name) and joins res.partner.bank for
// IBAN data so the suggester can fuzzy-match by IBAN too.
func FetchPartners(db *Database, uid int) (*PartnersFile, error) {
	rows, err := SearchReadAllMaps(db, uid, "res.partner",
		[]interface{}{[]interface{}{"active", "=", true}},
		[]string{"id", "name"},
		"id asc",
	)
	if err != nil {
		return nil, err
	}
	idx := &PartnersFile{
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		ByID:      map[int]*Partner{},
		ByIBAN:    map[string]int{},
	}
	for _, r := range rows {
		id := Int(r["id"])
		if id == 0 {
			continue
		}
		idx.ByID[id] = &Partner{ID: id, Name: Str(r["name"])}
	}

	banks, err := SearchReadAllMaps(db, uid, "res.partner.bank",
		[]interface{}{},
		[]string{"id", "partner_id", "acc_number", "sanitized_acc_number"},
		"id asc",
	)
	if err == nil {
		for _, b := range banks {
			pid := FieldID(b["partner_id"])
			if pid == 0 {
				continue
			}
			iban := Str(b["sanitized_acc_number"])
			if iban == "" {
				iban = Str(b["acc_number"])
			}
			if iban == "" {
				continue
			}
			if p, ok := idx.ByID[pid]; ok {
				p.IBANs = append(p.IBANs, iban)
			}
			idx.ByIBAN[iban] = pid
		}
	}
	idx.Count = len(idx.ByID)
	return idx, nil
}

// WritePartners persists the partner index.
func WritePartners(dbname string, idx *PartnersFile) error {
	if err := EnsureCacheDirs(dbname); err != nil {
		return err
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(CacheDir(dbname), "partners.json"), data, 0600)
}

func writeLastSync(dbname string, s *LastSyncFile) error {
	if err := EnsureCacheDirs(dbname); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(LastSyncPath(dbname), data, 0600)
}

// LoadLastSync reads the per-DB sync cursor. Empty struct when missing.
func LoadLastSync(dbname string) *LastSyncFile {
	data, err := os.ReadFile(LastSyncPath(dbname))
	if err != nil {
		return &LastSyncFile{}
	}
	var s LastSyncFile
	if err := json.Unmarshal(data, &s); err != nil {
		return &LastSyncFile{}
	}
	return &s
}

func printPullHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo pull%s — refresh local cache from the active Odoo database

%sUSAGE%s
  %sodoo pull%s              Pull journals list + favorite-journal lines +
                          open invoices + open bills + partner index
  %sodoo pull --all%s        Pull lines for EVERY journal (default: favorites only)
  %sodoo pull -v%s           Verbose progress

%sBEHAVIOUR%s
  Read-only against Odoo. Writes JSON files under
  ~/.odoo/cache/<dbname>/. Run after %sodoo setup%s and whenever the
  trusted-host data is stale.

%sCACHE LAYOUT%s
  journals/list.json        Every journal (id, name, code, type, currency)
  journals/<id>.json        Per-favorite-journal bank statement lines
  invoices.json             Open + partially-paid out_invoice / out_refund
  bills.json                Open + partially-paid in_invoice / in_refund
  partners.json             id → name + IBANs
  _last_sync.json           Timestamps + counts for the dashboard

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
	)
}
