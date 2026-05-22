package cmd

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
)

// reconLite is the slim line shape the same-account matcher works
// with. Narrower than AccountMoveLine because we only need the
// fields the matcher reads (amount + date + partner + identity) and
// the bits the picker needs to identify the line for the operator
// (move reference + move_type + line memo).
type reconLite struct {
	ID          int
	MoveID      int
	MoveName    string // parent move name, e.g. "INV/2025/00045" or "BNK1/2025/01/0001"
	MoveType    string // "out_invoice" / "in_invoice" / "entry" / …
	MoveRef     string // move-level external reference (often the bank memo)
	Date        string
	Name        string // line label — invoice line description OR bank statement memo
	Debit       float64
	Credit      float64
	Balance     float64
	PartnerID   int
	PartnerName string
}

// reconPlan is the per-line outcome of the matcher: the line itself,
// its ranked candidate list, and whether the top candidate is a
// strict partner match or a relaxed (amount-only) one.
type reconPlan struct {
	Line          reconLite
	Candidates    []reconLite
	PartnerStrict bool // true when the candidates were filtered to a matching partner
}

// reconPair is a finalised debit↔credit pairing — the unit that
// account.move.line.reconcile actually consumes.
type reconPair struct {
	A           reconLite // debit side
	B           reconLite // credit side
	Strict      bool      // partner-match on both sides
	WorstDelta  int       // |date(A) − date(B)|, in days
}

// ReconcileCmd is `odoo reconcile --account <code>` — same-account
// reconciliation. Pairs unreconciled debit/credit move-lines on a
// single GL account by amount (strict) + date proximity + partner
// identity. The partner condition is relaxed per-line when the
// strict pass yields no candidates.
//
//	safe pairs   — mutual 1:1 candidate (each line is the other's
//	               only candidate). Applied in batch with --yes.
//	ambiguous    — line has >1 candidate, or its candidate has >1
//	               competing pickers. Needs the TUI (`-i`) to resolve.
//	no-match     — no opposite-sign line with the same |amount|
//	               anywhere on the account.
//
// The applied write is one account.move.line.reconcile RPC per pair
// (Odoo accepts only same-account, opposite-sign reconciles in a
// single call — exactly the pair shape).
func ReconcileCmd(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printReconcileCmdHelp()
		return nil
	}
	accSpec := strings.TrimSpace(GetOption(args, "--account", "-a"))
	if accSpec == "" {
		return fmt.Errorf("usage: odoo reconcile --account <code|id>")
	}
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)
	PrintActiveDBBanner(db.Name)

	interactive := HasFlag(args, "-i", "--interactive")
	dryRun := HasFlag(args, "--dry-run")
	assumeYes := HasFlag(args, "--yes", "-y")
	verbose := HasFlag(args, "-v", "--verbose")
	if dryRun {
		assumeYes = false
	}

	fmt.Printf("\n%s● Authenticating against %s …%s\n", Fmt.Dim, db.URL, Fmt.Reset)
	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}
	acc, err := ResolveAccount(db, uid, accSpec)
	if err != nil {
		return err
	}

	debits, credits, err := fetchUnreconciledLines(db, uid, acc.ID)
	if err != nil {
		return err
	}
	if len(debits)+len(credits) == 0 {
		fmt.Printf("\n%s● No unreconciled lines on account %s — nothing to do.%s\n\n", Fmt.Dim, acc.Code, Fmt.Reset)
		return nil
	}

	fmt.Printf("\n%sReconcile account %s%s — %s%s%s · %d unreconciled (%d debit / %d credit)\n",
		Fmt.Bold, acc.Code, Fmt.Reset,
		Fmt.Dim, acc.Name, Fmt.Reset,
		len(debits)+len(credits), len(debits), len(credits))

	plans := buildReconPlans(debits, credits)
	pairs, ambiguousIDs, noneIDs := classifyMutualPairs(plans)
	fmt.Printf("  %ssafe pairs: %d · ambiguous: %d · no-match: %d%s\n\n",
		Fmt.Dim, len(pairs), len(ambiguousIDs), len(noneIDs), Fmt.Reset)

	if len(pairs) > 0 {
		fmt.Printf("%sSafe pairs%s\n", Fmt.Bold, Fmt.Reset)
		printPairs(pairs, verbose)
		fmt.Println()
	}
	if verbose && len(ambiguousIDs) > 0 {
		fmt.Printf("%sAmbiguous (need -i)%s\n", Fmt.Bold, Fmt.Reset)
		printAmbiguous(plans, ambiguousIDs)
		fmt.Println()
	}

	if interactive {
		if len(ambiguousIDs) == 0 {
			fmt.Printf("%sNothing ambiguous — every line is either safe or has no candidate.%s\n\n", Fmt.Dim, Fmt.Reset)
			// Fall through to safe-pair apply if --yes is also passed.
		} else {
			if err := reconcileAccountInteractive(db, uid, acc, plans, ambiguousIDs); err != nil {
				return err
			}
		}
	}

	if len(pairs) == 0 {
		fmt.Printf("%sNothing safe to apply.%s", Fmt.Dim, Fmt.Reset)
		if len(ambiguousIDs) > 0 {
			fmt.Printf(" Re-run with %s-i%s to resolve %d ambiguous line%s.",
				Fmt.Cyan, Fmt.Reset, len(ambiguousIDs), pluralS(len(ambiguousIDs)))
		}
		fmt.Println()
		fmt.Println()
		return nil
	}

	if dryRun {
		fmt.Printf("%s(dry-run — re-run with --yes to apply.)%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}
	if !assumeYes {
		if !isTTY() {
			return fmt.Errorf("refusing to write on a non-TTY without --yes")
		}
		fmt.Printf("%sReconcile %d safe pair%s on %s?%s [Y/n] ",
			Fmt.Bold, len(pairs), pluralS(len(pairs)), db.Host(), Fmt.Reset)
		resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		resp = strings.ToLower(strings.TrimSpace(resp))
		if resp == "n" || resp == "no" {
			fmt.Println("  cancelled.")
			return nil
		}
	}

	var applied, failed int
	for _, p := range pairs {
		_, err := Exec(db.URL, db.DB, uid, db.Password,
			"account.move.line", "reconcile",
			[]interface{}{[]interface{}{p.A.ID, p.B.ID}}, nil)
		if err != nil {
			failed++
			fmt.Printf("  %s✗%s #%d ↔ #%d: %v\n", Fmt.Red, Fmt.Reset, p.A.ID, p.B.ID, err)
			continue
		}
		applied++
		if verbose {
			fmt.Printf("  %s✓%s #%d ↔ #%d · %s · %s\n",
				Fmt.Green, Fmt.Reset, p.A.ID, p.B.ID,
				FmtEUR(p.A.Debit+p.A.Credit),
				FirstNonEmpty(p.A.PartnerName, p.B.PartnerName))
		}
	}
	fmt.Printf("\n%sReconciled %d pair%s%s", Fmt.Green, applied, pluralS(applied), Fmt.Reset)
	if failed > 0 {
		fmt.Printf("  %s(%d failed)%s", Fmt.Red, failed, Fmt.Reset)
	}
	fmt.Println()
	fmt.Println()
	return nil
}

// fetchUnreconciledLines reads every posted, unreconciled move-line
// on the account and splits into debits/credits. After the line
// fetch, the unique parent moves are read once more to fill in
// MoveType and MoveRef so the picker can identify each line as an
// invoice/bill/credit-note vs. a misc/bank journal entry.
func fetchUnreconciledLines(db *Database, uid int, accountID int) ([]reconLite, []reconLite, error) {
	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{
			[]interface{}{"account_id", "=", accountID},
			[]interface{}{"reconciled", "=", false},
			[]interface{}{"parent_state", "=", "posted"},
		},
		[]string{"id", "move_id", "date", "name", "debit", "credit", "balance", "partner_id"},
		"date asc, id asc",
	)
	if err != nil {
		return nil, nil, fmt.Errorf("read move lines: %v", err)
	}
	var debits, credits []reconLite
	moveIDSet := map[int]struct{}{}
	for _, r := range rows {
		l := reconLite{
			ID:          Int(r["id"]),
			MoveID:      FieldID(r["move_id"]),
			MoveName:    FieldName(r["move_id"]),
			Date:        Str(r["date"]),
			Name:        Str(r["name"]),
			Debit:       Float(r["debit"]),
			Credit:      Float(r["credit"]),
			Balance:     Float(r["balance"]),
			PartnerID:   FieldID(r["partner_id"]),
			PartnerName: FieldName(r["partner_id"]),
		}
		if l.MoveID > 0 {
			moveIDSet[l.MoveID] = struct{}{}
		}
		switch {
		case l.Debit > 0.005:
			debits = append(debits, l)
		case l.Credit > 0.005:
			credits = append(credits, l)
		}
	}

	moveMeta := fetchMoveMeta(db, uid, moveIDSet)
	for i := range debits {
		if m, ok := moveMeta[debits[i].MoveID]; ok {
			debits[i].MoveType = m.MoveType
			debits[i].MoveRef = m.Ref
		}
	}
	for i := range credits {
		if m, ok := moveMeta[credits[i].MoveID]; ok {
			credits[i].MoveType = m.MoveType
			credits[i].MoveRef = m.Ref
		}
	}
	return debits, credits, nil
}

// reconMoveMeta is the per-move enrichment used to label lines in
// the picker. Best-effort — fetch errors leave the map empty and the
// picker falls back to whatever it has on the line itself.
type reconMoveMeta struct {
	MoveType string
	Ref      string
}

func fetchMoveMeta(db *Database, uid int, ids map[int]struct{}) map[int]reconMoveMeta {
	out := map[int]reconMoveMeta{}
	if len(ids) == 0 {
		return out
	}
	arr := make([]interface{}, 0, len(ids))
	for id := range ids {
		arr = append(arr, id)
	}
	rows, err := SearchReadAllMaps(db, uid, "account.move",
		[]interface{}{[]interface{}{"id", "in", arr}},
		[]string{"id", "move_type", "ref"},
		"id asc",
	)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[Int(r["id"])] = reconMoveMeta{
			MoveType: Str(r["move_type"]),
			Ref:      Str(r["ref"]),
		}
	}
	return out
}

// buildReconPlans walks each side, finds opposite-sign candidates
// with same |amount|, applies the partner-strict filter (with
// relaxation), and ranks by date proximity.
func buildReconPlans(debits, credits []reconLite) map[int]reconPlan {
	plans := make(map[int]reconPlan, len(debits)+len(credits))
	plan := func(l reconLite, opposite []reconLite) reconPlan {
		amt := math.Abs(l.Debit - l.Credit)
		candidates := make([]reconLite, 0)
		for _, c := range opposite {
			oAmt := math.Abs(c.Debit - c.Credit)
			if math.Abs(amt-oAmt) > 0.01 {
				continue
			}
			candidates = append(candidates, c)
		}
		// Partner-strict filter (relax when it empties the set).
		strict := candidates
		partnerStrict := false
		if l.PartnerID > 0 {
			filtered := make([]reconLite, 0, len(candidates))
			for _, c := range candidates {
				if c.PartnerID == l.PartnerID {
					filtered = append(filtered, c)
				}
			}
			if len(filtered) > 0 {
				strict = filtered
				partnerStrict = true
			}
		}
		// Sort by absolute date delta (closest first).
		sort.SliceStable(strict, func(i, j int) bool {
			return absDaysBetween(strict[i].Date, l.Date) < absDaysBetween(strict[j].Date, l.Date)
		})
		return reconPlan{Line: l, Candidates: strict, PartnerStrict: partnerStrict}
	}
	for _, d := range debits {
		plans[d.ID] = plan(d, credits)
	}
	for _, c := range credits {
		plans[c.ID] = plan(c, debits)
	}
	return plans
}

// classifyMutualPairs walks the plans and emits the mutual 1:1
// candidate pairs (truly safe) along with the ambiguous and
// no-match line ids.
func classifyMutualPairs(plans map[int]reconPlan) (pairs []reconPair, ambiguous, none []int) {
	used := map[int]bool{}
	// Deterministic walk order: by line id.
	ids := make([]int, 0, len(plans))
	for id := range plans {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		if used[id] {
			continue
		}
		p := plans[id]
		switch len(p.Candidates) {
		case 0:
			none = append(none, id)
			continue
		case 1:
			// Check mutuality: the candidate must also have this
			// line as its only candidate.
			cand := p.Candidates[0]
			cPlan, ok := plans[cand.ID]
			if !ok || used[cand.ID] {
				ambiguous = append(ambiguous, id)
				continue
			}
			if len(cPlan.Candidates) != 1 || cPlan.Candidates[0].ID != id {
				ambiguous = append(ambiguous, id)
				continue
			}
			a, b := p.Line, cand
			if a.Credit > 0 { // canonicalise: A is debit, B is credit
				a, b = b, a
			}
			pairs = append(pairs, reconPair{
				A:          a,
				B:          b,
				Strict:     p.PartnerStrict && cPlan.PartnerStrict,
				WorstDelta: absDaysBetween(a.Date, b.Date),
			})
			used[id] = true
			used[cand.ID] = true
		default:
			ambiguous = append(ambiguous, id)
		}
	}
	return pairs, ambiguous, none
}

// reconLineKind returns a short human label derived from move_type.
// "Transaction" covers `entry` (misc/bank/journal entries) — the
// catch-all that isn't an invoice/bill/refund.
func reconLineKind(moveType string) string {
	switch moveType {
	case "out_invoice":
		return "Invoice"
	case "in_invoice":
		return "Bill"
	case "out_refund":
		return "Credit Note"
	case "in_refund":
		return "Refund"
	case "entry", "":
		return "Transaction"
	}
	return "Entry"
}

// reconLineMemo returns the most useful description text for a line:
// the line's own name (usually the invoice line description or the
// bank statement memo), falling back to the move-level external ref
// when the line name is empty.
func reconLineMemo(l reconLite) string {
	if s := strings.TrimSpace(l.Name); s != "" {
		return CollapseWhitespace(s)
	}
	return CollapseWhitespace(l.MoveRef)
}

// reconLineRef returns "<Kind> <MoveName>" (e.g. "Invoice INV/2025/00045"
// or "Transaction BNK1/2025/01/0001"). Falls back to just the kind
// when MoveName is empty.
func reconLineRef(l reconLite) string {
	kind := reconLineKind(l.MoveType)
	if l.MoveName == "" {
		return kind
	}
	return kind + " " + l.MoveName
}

// printLineContext writes the two-line "what is this move-line"
// detail block used by the picker — one row for ref + partner, a
// second row for the full memo when it's present and distinct.
func printLineContext(l reconLite, indent string) {
	ref := reconLineRef(l)
	partner := strings.TrimSpace(l.PartnerName)
	memo := reconLineMemo(l)

	switch {
	case partner != "":
		fmt.Printf("%s%s%s · %s%s\n", indent, Fmt.Bold, ref, partner, Fmt.Reset)
	default:
		fmt.Printf("%s%s%s%s\n", indent, Fmt.Bold, ref, Fmt.Reset)
	}
	// Suppress memo when it just repeats the move name (common for
	// AR/AP receivable lines whose `name` is the invoice number).
	if memo != "" && memo != l.MoveName {
		fmt.Printf("%s%s%s%s\n", indent, Fmt.Dim, memo, Fmt.Reset)
	}
}

func printPairs(pairs []reconPair, verbose bool) {
	limit := 15
	if verbose {
		limit = len(pairs)
	}
	if limit > len(pairs) {
		limit = len(pairs)
	}
	for i := 0; i < limit; i++ {
		p := pairs[i]
		strict := " "
		if p.Strict {
			strict = "★"
		}
		fmt.Printf("  %s%s%s #%-6d %s · %s  %s↔%s  #%-6d %s · %s   Δ%dd\n",
			Fmt.Green, strict, Fmt.Reset,
			p.A.ID, p.A.Date, FmtEUR(p.A.Debit),
			Fmt.Dim, Fmt.Reset,
			p.B.ID, p.B.Date, FmtEUR(p.B.Credit),
			p.WorstDelta)
		// One detail line per side so the operator can read the full
		// memo / invoice reference without the row truncating.
		printLineContext(p.A, "       ↳ debit  ")
		printLineContext(p.B, "       ↳ credit ")
	}
	if limit < len(pairs) {
		fmt.Printf("  %s… and %d more (pass -v to list every pair)%s\n",
			Fmt.Dim, len(pairs)-limit, Fmt.Reset)
	}
}

func printAmbiguous(plans map[int]reconPlan, ids []int) {
	limit := 10
	if limit > len(ids) {
		limit = len(ids)
	}
	for i := 0; i < limit; i++ {
		p := plans[ids[i]]
		side := "debit"
		if p.Line.Credit > 0 {
			side = "credit"
		}
		fmt.Printf("  %s?%s #%-6d %s · %s %s · %d candidate%s\n",
			Fmt.Yellow, Fmt.Reset,
			p.Line.ID, p.Line.Date,
			FmtEUR(p.Line.Debit+p.Line.Credit), side,
			len(p.Candidates), pluralS(len(p.Candidates)))
		printLineContext(p.Line, "       ")
	}
	if limit < len(ids) {
		fmt.Printf("  %s… and %d more%s\n", Fmt.Dim, len(ids)-limit, Fmt.Reset)
	}
}

// reconcileAccountInteractive walks the ambiguous lines and lets
// the operator pick a candidate (or skip) one at a time. Picks are
// applied immediately so a Ctrl-C mid-walk leaves the rest in place.
//
// Line-based prompter (not bubbletea) — sticks with one terminal
// frame and reads single tokens. The bigger bubbletea TUI is the
// journals `reconcile -i` flow; this command's complement is simpler
// because the candidate list is small (1-N opposite-sign lines on
// the same account, not the chb-style invoice/bill picker).
func reconcileAccountInteractive(db *Database, uid int, acc *Account, plans map[int]reconPlan, ambiguousIDs []int) error {
	reader := bufio.NewReader(os.Stdin)
	resolved := map[int]bool{}
	picked, skipped := 0, 0

	fmt.Printf("\n%s── interactive (%d ambiguous) ──%s\n", Fmt.Bold, len(ambiguousIDs), Fmt.Reset)
	fmt.Printf("%spick a number to attach · s skip · q quit (already-applied picks stay)%s\n\n", Fmt.Dim, Fmt.Reset)

	for i, id := range ambiguousIDs {
		if resolved[id] {
			continue
		}
		p := plans[id]
		side := "debit"
		if p.Line.Credit > 0 {
			side = "credit"
		}
		fmt.Printf("%s[%d/%d]%s #%d · %s · %s %s\n",
			Fmt.Bold, i+1, len(ambiguousIDs), Fmt.Reset,
			p.Line.ID, p.Line.Date,
			FmtEUR(p.Line.Debit+p.Line.Credit), side)
		printLineContext(p.Line, "       ")
		if !p.PartnerStrict && p.Line.PartnerID > 0 {
			fmt.Printf("  %s(no partner-strict candidate — showing amount-only matches)%s\n", Fmt.Dim, Fmt.Reset)
		}
		// Show candidates, capped to 9 so single-digit input works.
		candLimit := len(p.Candidates)
		if candLimit > 9 {
			candLimit = 9
		}
		for j := 0; j < candLimit; j++ {
			c := p.Candidates[j]
			delta := absDaysBetween(c.Date, p.Line.Date)
			tag := " "
			if c.PartnerID == p.Line.PartnerID && p.Line.PartnerID > 0 {
				tag = "★"
			}
			cside := "debit"
			if c.Credit > 0 {
				cside = "credit"
			}
			fmt.Printf("\n  %s%d%s %s  %s · %s %s · Δ%dd\n",
				Fmt.Cyan, j+1, Fmt.Reset, tag,
				c.Date, FmtEUR(c.Debit+c.Credit), cside, delta)
			printLineContext(c, "      ")
		}
		if candLimit < len(p.Candidates) {
			fmt.Printf("  %s(%d more candidates not shown — narrow with --search via the chb side first)%s\n",
				Fmt.Dim, len(p.Candidates)-candLimit, Fmt.Reset)
		}

		fmt.Printf("  > ")
		raw, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println()
			fmt.Printf("%s(stdin closed — stopping after %d picked / %d skipped)%s\n",
				Fmt.Dim, picked, skipped, Fmt.Reset)
			break
		}
		ans := strings.ToLower(strings.TrimSpace(raw))
		switch ans {
		case "q", "quit", "exit":
			fmt.Printf("%s(quit — %d picked, %d skipped, %d remaining)%s\n\n",
				Fmt.Dim, picked, skipped, len(ambiguousIDs)-i-1, Fmt.Reset)
			return nil
		case "", "s", "skip":
			skipped++
			fmt.Printf("  %sskipped.%s\n\n", Fmt.Dim, Fmt.Reset)
			continue
		}
		var choice int
		if _, perr := fmt.Sscanf(ans, "%d", &choice); perr != nil || choice < 1 || choice > candLimit {
			fmt.Printf("  %s? not understood (expected 1-%d, s, or q)%s\n\n", Fmt.Yellow, candLimit, Fmt.Reset)
			// Re-queue: stay on this line.
			i--
			continue
		}
		cand := p.Candidates[choice-1]
		_, rerr := Exec(db.URL, db.DB, uid, db.Password,
			"account.move.line", "reconcile",
			[]interface{}{[]interface{}{p.Line.ID, cand.ID}}, nil)
		if rerr != nil {
			fmt.Printf("  %s✗%s reconcile #%d ↔ #%d: %v\n\n", Fmt.Red, Fmt.Reset, p.Line.ID, cand.ID, rerr)
			continue
		}
		picked++
		resolved[id] = true
		resolved[cand.ID] = true
		fmt.Printf("  %s✓%s reconciled #%d ↔ #%d\n\n", Fmt.Green, Fmt.Reset, p.Line.ID, cand.ID)
	}
	fmt.Printf("\n%sInteractive done — %d picked, %d skipped on %s.%s\n\n",
		Fmt.Green, picked, skipped, acc.Code, Fmt.Reset)
	return nil
}

func printReconcileCmdHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo reconcile --account <code|id>%s — pair unreconciled debits/credits on a GL account

%sUSAGE%s
  %sodoo reconcile --account 400000%s             Preview (no writes)
  %sodoo reconcile --account 400000 --yes%s       Apply safe pairs
  %sodoo reconcile --account 400000 -i%s          TUI for ambiguous lines
  %sodoo reconcile -a 400000 -v%s                 List every safe + ambiguous

%sMATCHER%s
  For each unreconciled posted move-line on the account:
    1. Find opposite-sign lines with the same |amount| (±0.01).
    2. Filter to lines whose partner matches; if that empties the set,
       relax the partner condition (amount-only match).
    3. Sort surviving candidates by date proximity.

  A pair is %ssafe%s when both sides have each other as their ONLY
  candidate (mutual 1:1). Anything else is %sambiguous%s.

%sCLASSIFICATION%s
  %ssafe%s        Mutual 1:1 candidate. ★ marks pairs where the
              partner matched on both sides (highest confidence).
  %sambiguous%s   Line has >1 candidate, or its candidate has competing
              pickers. Needs %s-i%s to resolve.
  %sno-match%s    No opposite-sign line with the same amount on the
              account. Probably needs manual investigation.

%sBEHAVIOUR%s
  Default is dry-run preview. %s--yes%s applies safe pairs in batch via
  account.move.line.reconcile (one RPC per pair). %s-i%s opens a TUI for
  the ambiguous bucket — picks are applied as you confirm them.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Green, f.Reset, f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Green, f.Reset,
		f.Yellow, f.Reset, f.Cyan, f.Reset,
		f.Red, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset, f.Cyan, f.Reset,
	)
}
