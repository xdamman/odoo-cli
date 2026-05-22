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

// pipedLineID is the minimal stdin record shape for the pipe-based
// commands (unreconcile, assign). Only `id` is required; everything
// else from `odoo account --jsonl` is ignored.
type pipedLineID struct {
	ID int `json:"id"`
}

// UnreconcileFromStdin is the top-level `odoo unreconcile` — reads
// JSONL on stdin (typically piped from `odoo account <code>`),
// collects every account.partial.reconcile linking those lines, and
// unlinks them in one Odoo call.
//
// The per-journal/account form lives at `odoo journals <id>
// unreconcile --account <code>` — different shape, different name
// inside the cmd package (Unreconcile). This stdin form is what
// you reach for in pipelines.
func UnreconcileFromStdin(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printUnreconcilePipeHelp()
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
	if dryRun {
		assumeYes = false
	}

	if term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("expected JSONL on stdin — pipe move-line ids (e.g. `odoo account 400000 --jsonl | odoo unreconcile`)")
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

	// Fetch matched_debit_ids + matched_credit_ids for every input
	// line in ONE call. Lines that aren't reconciled contribute
	// nothing to the partial set, so they're silently no-ops.
	idsAny := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		idsAny = append(idsAny, id)
	}
	rows, err := SearchReadAllMaps(db, uid, "account.move.line",
		[]interface{}{[]interface{}{"id", "in", idsAny}},
		[]string{"id", "date", "name", "debit", "credit", "balance",
			"reconciled", "matched_debit_ids", "matched_credit_ids", "account_id"},
		"date asc, id asc",
	)
	if err != nil {
		return fmt.Errorf("read move lines: %v", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("none of the %d piped ids match an account.move.line", len(ids))
	}

	partials := map[int]bool{}
	type sample struct {
		ID, MoveID  int
		Date, Name  string
		Balance     float64
		Reconciled  bool
		Account     string
	}
	samples := make([]sample, 0, len(rows))
	var reconciledCount int
	for _, r := range rows {
		recon := Bool(r["reconciled"])
		if recon {
			reconciledCount++
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
		samples = append(samples, sample{
			ID:         Int(r["id"]),
			Date:       Str(r["date"]),
			Name:       Str(r["name"]),
			Balance:    Float(r["balance"]),
			Reconciled: recon,
			Account:    FieldName(r["account_id"]),
		})
	}

	fmt.Printf("\n%sUnreconcile %d move-line%s%s — %d reconciled · %s%d partial-reconcile record%s%s to unlink\n\n",
		Fmt.Bold, len(rows), pluralS(len(rows)), Fmt.Reset,
		reconciledCount,
		Fmt.Bold, len(partials), pluralS(len(partials)), Fmt.Reset)

	previewLimit := 10
	if verbose {
		previewLimit = len(samples)
	}
	if previewLimit > len(samples) {
		previewLimit = len(samples)
	}
	for i := 0; i < previewLimit; i++ {
		s := samples[i]
		recIcon := " "
		if s.Reconciled {
			recIcon = "✓"
		}
		fmt.Printf("  %s%s line #%d · %s · %s · %s · %s%s\n",
			Fmt.Dim, recIcon, s.ID, s.Date,
			FmtEURSigned(s.Balance),
			Truncate(s.Account, 28),
			Truncate(s.Name, 40), Fmt.Reset)
	}
	if previewLimit < len(samples) {
		fmt.Printf("  %s… and %d more (pass -v to list every line)%s\n",
			Fmt.Dim, len(samples)-previewLimit, Fmt.Reset)
	}
	fmt.Println()

	if len(partials) == 0 {
		fmt.Printf("%s● No partial-reconcile records on those lines — nothing to unlink.%s\n\n",
			Fmt.Dim, Fmt.Reset)
		return nil
	}
	if dryRun || !assumeYes {
		fmt.Printf("%s(dry-run — re-run with --yes to apply.)%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}

	partialIDs := make([]interface{}, 0, len(partials))
	for id := range partials {
		partialIDs = append(partialIDs, id)
	}
	if _, err := Exec(db.URL, db.DB, uid, db.Password,
		"account.partial.reconcile", "unlink",
		[]interface{}{partialIDs}, nil); err != nil {
		return fmt.Errorf("unlink partials: %v", err)
	}
	fmt.Printf("%s✓ Unlinked %d partial-reconcile record%s on %s%s\n\n",
		Fmt.Green, len(partials), pluralS(len(partials)), db.Host(), Fmt.Reset)
	return nil
}

// readPipedLineIDs parses JSONL on r — only the `id` field is
// looked at. Blank lines skipped; malformed records warned + skipped.
// Dedupes so re-piping the same record twice does no harm.
func readPipedLineIDs(r io.Reader) ([]int, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)
	seen := map[int]bool{}
	out := make([]int, 0)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec pipedLineID
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			fmt.Fprintf(os.Stderr, "  %s⚠ line %d: %v — skipped%s\n", Fmt.Yellow, lineNum, err, Fmt.Reset)
			continue
		}
		if rec.ID <= 0 {
			fmt.Fprintf(os.Stderr, "  %s⚠ line %d: missing or non-positive `id` — skipped%s\n", Fmt.Yellow, lineNum, Fmt.Reset)
			continue
		}
		if seen[rec.ID] {
			continue
		}
		seen[rec.ID] = true
		out = append(out, rec.ID)
	}
	return out, scanner.Err()
}

func printUnreconcilePipeHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo unreconcile%s — unreconcile move-lines piped on stdin

%sUSAGE%s
  %sodoo account 400000 --jsonl | odoo unreconcile%s         Dry-run preview
  %sodoo account 400000 --jsonl | odoo unreconcile --yes%s   Apply
  %s… | jq 'select(.reconciled)' | odoo unreconcile --yes%s  Filter first

%sBEHAVIOUR%s
  Reads JSONL on stdin (one record per line). Only the %sid%s field is
  used — must be an account.move.line id (as emitted by %sodoo account
  <code> --jsonl%s). Other fields are ignored, so piping straight from
  %sodoo account%s works without jq.

  Collects every account.partial.reconcile linking those lines via
  their matched_debit_ids / matched_credit_ids m2m, and unlinks them
  in a single Odoo call.

  The per-journal/account batch form lives at %sodoo journals <id>
  unreconcile --account <code>%s. Use that when you want to wipe every
  reconciliation on a journal+account without an intermediate filter.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}
