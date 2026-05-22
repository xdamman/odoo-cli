package cmd

import (
	"math"
	"sort"
	"strings"
	"time"
)

// SuggestDirection is the side of the bank ledger the suggester is
// looking for. Incoming = positive bank-line amount = matches
// customer invoices (out_invoice). Outgoing = negative bank-line
// amount = matches vendor bills (in_invoice).
type SuggestDirection int

const (
	SuggestIncoming SuggestDirection = iota
	SuggestOutgoing
)

// Suggestion is one ranked candidate for a reconcile pick. Used by
// both the bank-line-side flow (SuggestForBankLine: starting from
// an unreconciled bank statement line, find the matching invoice or
// bill) and the future invoice-side flow.
//
// The flat scalars are populated for every Suggestion so a picker
// can render a uniform table. The typed payload (Move / Line) is
// also kept so the apply path has everything it needs to call the
// RPC without re-deriving anything from the cache.
type Suggestion struct {
	Kind string // "invoice" / "bill" / "bank-line"

	// Common scalar projection.
	ID           int
	Date         string // YYYY-MM-DD
	Amount       float64 // absolute, in EUR
	Currency     string
	Partner      string
	PartnerID    int
	Reference    string // invoice number, or bank line payment_ref
	Description  string // first line item title, or bank line payment_ref
	PaymentState string // moves only: not_paid / in_payment / partial / paid

	// Move-side payload (Kind ∈ {"invoice","bill"}). Empty for
	// bank-line suggestions.
	Move Invoice

	// Bank-line-side payload (Kind == "bank-line"). Empty otherwise.
	Line      JournalLine
	JournalID int

	// Scoring + status.
	PartnerMatch    bool // partner-id equality OR fuzzy token match
	DaysDelta       int  // |candidate date − query date|, in days
	AlreadyAttached bool // moves: paymentState ∈ {paid,in_payment,partial}
	                    // bank lines: IsReconciled
}

// SuggestForBankLine returns ranked invoice/bill candidates for a
// given bank statement line. The suggester walks the local cache
// (invoices.json + bills.json) and applies the two-pass widening
// rule used everywhere in this CLI family:
//
//	Pass 1 — amount + direction + UNRECONCILED only.
//	         If non-empty: return.
//	Pass 2 — same filter but including already-attached
//	         (paid / in_payment / partial) candidates,
//	         each with AlreadyAttached=true so the picker
//	         can offer unreconcile + reattach.
//
// Sort: partner-match first, then absolute date proximity. The
// suggestions returned have un-reconciled candidates strictly
// before AlreadyAttached ones; the picker default cursor lands on
// the first unattached candidate via FirstUnattachedIndex.
//
// All input comes from the local cache; no RPCs. Run `odoo pull`
// to refresh first if results look stale.
func SuggestForBankLine(dbname string, line JournalLine) []Suggestion {
	if line.Amount == 0 {
		return nil
	}
	dir := SuggestIncoming
	wantKind := "invoice"
	if line.Amount < 0 {
		dir = SuggestOutgoing
		wantKind = "bill"
	}
	cands := loadMoveCandidates(dbname, wantKind)
	if len(cands) == 0 {
		return nil
	}

	partnerIdx := loadPartnersIndex(dbname)
	partnerTokens := nameTokens(resolvePartnerName(partnerIdx, line.PartnerID))

	open := make([]Suggestion, 0, 8)
	attached := make([]Suggestion, 0, 8)
	for _, inv := range cands {
		// Amount match within ±0.01 EUR (residual when open, total
		// when paid — Odoo zeros residual on payment).
		amt := math.Abs(inv.Residual)
		if amt < 0.005 {
			amt = math.Abs(inv.Amount)
		}
		if math.Abs(amt-math.Abs(line.Amount)) > 0.01 {
			continue
		}
		// Direction gate via move_type.
		if !moveTypeMatchesDirection(inv.MoveType, dir) {
			continue
		}
		s := buildMoveSuggestion(inv, line.Date, partnerIdx, partnerTokens, line.PartnerID)
		if s.AlreadyAttached {
			attached = append(attached, s)
		} else {
			open = append(open, s)
		}
	}

	sortSuggestions(open)
	if len(open) > 0 {
		return open
	}
	sortSuggestions(attached)
	return attached
}

// loadMoveCandidates returns either the cached invoices.json or
// bills.json, depending on wantKind.
func loadMoveCandidates(dbname, wantKind string) []Invoice {
	switch wantKind {
	case "invoice":
		if f, ok := readInvoicesFile(dbname, "invoices.json"); ok {
			return f.Invoices
		}
	case "bill":
		if f, ok := readInvoicesFile(dbname, "bills.json"); ok {
			return f.Invoices
		}
	}
	return nil
}

// moveTypeMatchesDirection reports whether the Odoo move_type
// matches the requested direction. out_invoice / out_refund =
// incoming bank flow (customer paying us); in_invoice / in_refund =
// outgoing (we pay vendors).
func moveTypeMatchesDirection(moveType string, dir SuggestDirection) bool {
	switch moveType {
	case "out_invoice", "out_refund":
		return dir == SuggestIncoming
	case "in_invoice", "in_refund":
		return dir == SuggestOutgoing
	}
	return false
}

// buildMoveSuggestion converts an Invoice + the bank line's date
// into a Suggestion, computing partner-match and date-delta along
// the way. partnerIdx is the local partner cache; partnerTokens is
// the fuzzy-match token set built from the bank line's partner.
func buildMoveSuggestion(inv Invoice, queryDate string, partnerIdx *PartnersFile, queryTokens []string, queryPartnerID int) Suggestion {
	delta := absDaysBetween(inv.Date, queryDate)
	if inv.InvoiceDate != "" {
		if d := absDaysBetween(inv.InvoiceDate, queryDate); d < delta || delta == bigDateDelta {
			delta = d
		}
	}
	residual := math.Abs(inv.Residual)
	if residual < 0.005 {
		residual = math.Abs(inv.Amount)
	}
	return Suggestion{
		Kind:            inv.MoveType, // "out_invoice" → not strictly "invoice", but kept as-is below
		ID:              inv.ID,
		Date:            FirstNonEmpty(inv.InvoiceDate, inv.Date),
		Amount:          residual,
		Currency:        inv.Currency,
		Partner:         inv.PartnerName,
		PartnerID:       inv.PartnerID,
		Reference:       inv.Name,
		Description:     inv.FirstLineItem,
		PaymentState:    inv.PaymentState,
		Move:            inv,
		PartnerMatch:    movePartnerMatches(inv, partnerIdx, queryTokens, queryPartnerID),
		DaysDelta:       delta,
		AlreadyAttached: !invoiceIsOpen(inv),
	}
}

// invoiceIsOpen returns true when the invoice / bill still owes
// money. PaymentState=paid OR explicit reversed → closed; anything
// else (not_paid / in_payment / partial / blank) → still open.
func invoiceIsOpen(inv Invoice) bool {
	ps := strings.ToLower(inv.PaymentState)
	if ps == "paid" || ps == "reversed" {
		return false
	}
	return true
}

// movePartnerMatches returns true when the candidate's partner is
// plausibly the same as the bank line's partner. Two checks:
//
//  1. strict partner_id equality (when both sides have a partner_id)
//  2. fuzzy token match: any ≥3-char token from the bank line's
//     partner name appearing in the candidate's partner name (and
//     vice versa).
func movePartnerMatches(inv Invoice, idx *PartnersFile, queryTokens []string, queryPartnerID int) bool {
	if queryPartnerID > 0 && inv.PartnerID == queryPartnerID {
		return true
	}
	if inv.PartnerName == "" || (len(queryTokens) == 0 && queryPartnerID == 0) {
		return false
	}
	candHay := strings.ToLower(inv.PartnerName)
	for _, t := range queryTokens {
		if strings.Contains(candHay, t) {
			return true
		}
	}
	candTokens := nameTokens(inv.PartnerName)
	queryName := ""
	if idx != nil && queryPartnerID > 0 {
		if p, ok := idx.ByID[queryPartnerID]; ok && p != nil {
			queryName = p.Name
		}
	}
	if queryName != "" {
		hay := strings.ToLower(queryName)
		for _, t := range candTokens {
			if strings.Contains(hay, t) {
				return true
			}
		}
	}
	return false
}

// nameTokens returns the lower-cased ≥3-char word tokens of a
// partner display name. Drops common legal suffixes (vzw, asbl,
// srl, …) and pure-numeric tokens so the fuzzy match isn't tripped
// by noise.
func nameTokens(name string) []string {
	stop := map[string]bool{
		"vzw": true, "asbl": true, "srl": true, "sprl": true, "sa": true,
		"nv": true, "bv": true, "bvba": true, "ltd": true, "llc": true,
		"inc": true, "the": true, "and": true, "co": true,
	}
	var out []string
	for _, t := range strings.Fields(strings.ToLower(name)) {
		t = strings.Trim(t, ",.;:'\"()[]{}")
		if len(t) < 3 || stop[t] {
			continue
		}
		allDigits := true
		for _, r := range t {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			continue
		}
		out = append(out, t)
	}
	return out
}

// resolvePartnerName looks up a partner's name by id in the local
// index, returning "" when no entry exists.
func resolvePartnerName(idx *PartnersFile, id int) string {
	if idx == nil || id == 0 {
		return ""
	}
	p, ok := idx.ByID[id]
	if !ok || p == nil {
		return ""
	}
	return p.Name
}

// loadPartnersIndex reads cache/<db>/partners.json. Returns nil
// when the file is missing — callers handle that gracefully.
func loadPartnersIndex(dbname string) *PartnersFile {
	idx := readPartners(dbname)
	if idx == nil || idx.ByID == nil {
		return &PartnersFile{ByID: map[int]*Partner{}, ByIBAN: map[string]int{}}
	}
	return idx
}

// readInvoicesFile loads either invoices.json or bills.json. Returns
// (nil, false) when missing.
func readInvoicesFile(dbname, name string) (*InvoicesFile, bool) {
	// loadInvoicesFile is the canonical name used by the pull
	// command's writer; mirror it here.
	return loadCachedInvoices(dbname, name)
}

// sortSuggestions orders by (partner-match desc, date-delta asc).
// Stable so cache order remains the final tiebreaker.
func sortSuggestions(s []Suggestion) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].PartnerMatch != s[j].PartnerMatch {
			return s[i].PartnerMatch
		}
		return s[i].DaysDelta < s[j].DaysDelta
	})
}

// FirstUnattachedIndex returns the lowest index where a Suggestion
// is NOT AlreadyAttached. Used by pickers to land the default
// cursor on a safe pick. Returns 0 when all entries are attached
// (which means the widening fallback fired) — the operator still
// gets a sensible default.
func FirstUnattachedIndex(s []Suggestion) int {
	for i, sg := range s {
		if !sg.AlreadyAttached {
			return i
		}
	}
	return 0
}

// SuggestionBadge returns the short "paid by … on …" hint
// rendered alongside an AlreadyAttached candidate in the picker.
// Falls back to "partner match" for unattached candidates whose
// partner matched, and "" otherwise.
func SuggestionBadge(s Suggestion) string {
	if s.AlreadyAttached {
		state := strings.ReplaceAll(s.PaymentState, "_", " ")
		if state == "" {
			state = "settled"
		}
		return state
	}
	if s.PartnerMatch {
		return "partner match"
	}
	return ""
}

const bigDateDelta = 1 << 30

// absDaysBetween returns |a − b| in days. Returns bigDateDelta when
// either side is unparseable so those rows sort last.
func absDaysBetween(a, b string) int {
	if len(a) < 10 || len(b) < 10 {
		return bigDateDelta
	}
	ta, e1 := time.Parse("2006-01-02", a[:10])
	tb, e2 := time.Parse("2006-01-02", b[:10])
	if e1 != nil || e2 != nil {
		return bigDateDelta
	}
	d := int(ta.Sub(tb).Hours() / 24)
	if d < 0 {
		return -d
	}
	return d
}

