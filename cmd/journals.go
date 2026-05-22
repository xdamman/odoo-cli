package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Journal is the cached shape — narrower than Odoo's full
// account.journal record, just what the list / detail views need.
type Journal struct {
	ID                int     `json:"id"`
	Name              string  `json:"name"`
	Code              string  `json:"code"`
	Type              string  `json:"type"`              // bank / cash / sale / purchase / general
	Currency          string  `json:"currency,omitempty"`
	DefaultAccountID  int     `json:"defaultAccountId,omitempty"`
	SuspenseAccountID int     `json:"suspenseAccountId,omitempty"`
	Active            bool    `json:"active"`
	IsFavorite        bool    `json:"isFavorite,omitempty"`  // mirrors Odoo's per-user account.journal.favorite_user_ids
	Balance           float64 `json:"balance,omitempty"`     // bank/cash journals: running balance
	LineCount         int     `json:"lineCount,omitempty"`   // last-known line count
	Updated           string  `json:"updated,omitempty"`     // RFC3339; when this record was last refreshed
}

// JournalsListFile is the on-disk shape of the cached journal list.
type JournalsListFile struct {
	FetchedAt string    `json:"fetchedAt"`
	Count     int       `json:"count"`
	Journals  []Journal `json:"journals"`
}

// Favorites is the per-DB favorites file.
type Favorites struct {
	Journals []int `json:"journals,omitempty"`
}

// Journals dispatches the `odoo journals …` family.
func Journals(args []string) error {
	// `--help` short-circuits at the TOP — but only when no
	// positional id + verb pair is present. Per-verb help printers
	// (e.g. `odoo journals 47 reconcile --help`) are reached via
	// the dispatch below so they print their own contextual help.
	hasIDPlusVerb := nthPositional(args, 2, "--db") != ""
	if !hasIDPlusVerb && HasFlag(args, "--help", "-h", "help") {
		printJournalsHelp()
		return nil
	}

	// Form: `odoo journals <id> [verb]`
	if id := FirstPositional(args, "--db", "--search", "-n", "--limit"); id != "" && !isFlag(id) {
		if jid, err := strconv.Atoi(id); err == nil {
			return journalDetailDispatch(jid, args)
		}
		// Not a numeric id — try resolving by code, then fall through
		// to list-with-search-by-name if no match.
		if jid := resolveJournalByCode(args, id); jid > 0 {
			return journalDetailDispatch(jid, args)
		}
		// First positional looked like a non-numeric, non-code arg —
		// surface a friendly error.
		return fmt.Errorf("journal %q not found (try `odoo journals --all` to list every journal, or `odoo journals --search %s`)", id, id)
	}

	return journalsList(args)
}

func journalDetailDispatch(jid int, args []string) error {
	// Position 2 (after the id) is the optional verb.
	verb := nthPositional(args, 2, "--db")
	switch verb {
	case "":
		return journalDetail(jid, args)
	case "favorite":
		return journalFavoriteToggle(jid, args, true)
	case "unfavorite":
		return journalFavoriteToggle(jid, args, false)
	case "reconcile":
		return journalReconcileStub(jid, args)
	case "unreconcile":
		return Unreconcile(jid, args)
	default:
		return fmt.Errorf("unknown journal verb %q (try: favorite / unfavorite / reconcile / unreconcile)", verb)
	}
}

// ── list ────────────────────────────────────────────────────────

func journalsList(args []string) error {
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)
	PrintActiveDBBanner(db.Name)

	all := HasFlag(args, "--all")
	search := strings.TrimSpace(GetOption(args, "--search"))
	journals, fromCache, err := loadOrFetchJournals(db)
	if err != nil {
		return err
	}

	fav, _ := LoadFavorites(db.Name)
	favSet := map[int]bool{}
	for _, id := range fav.Journals {
		favSet[id] = true
	}
	// IsFavorite from the cached Journal record (mirroring Odoo's
	// favorite_user_ids) takes precedence; the local favorites.json
	// is unioned in for backward compat until the operator's first
	// post-upgrade pull lands.
	for _, j := range journals {
		if j.IsFavorite {
			favSet[j.ID] = true
		}
	}

	filter := journals
	if search != "" {
		filter = filterJournals(filter, search)
	} else if !all {
		// Default: favorites only.
		filter = filterJournalsByFavorite(filter, favSet)
	}

	sort.SliceStable(filter, func(i, j int) bool { return strings.ToLower(filter[i].Name) < strings.ToLower(filter[j].Name) })

	noun := "journal"
	if !all && search == "" {
		noun = "favorite"
	}
	fmt.Printf("\n%s%d %s%s — %s%s%s\n\n", Fmt.Bold, len(filter), Pluralize(len(filter), noun, ""), Fmt.Reset,
		Fmt.Dim, journalsListSubtitle(all, search), Fmt.Reset)

	if len(filter) == 0 {
		hint := "Run `odoo journals --all` to list every journal, or `odoo journals <id> favorite` to add favorites."
		if search != "" {
			hint = "Try a different keyword or `--all` to widen the search."
		}
		fmt.Printf("  %s%s%s\n\n", Fmt.Dim, hint, Fmt.Reset)
		return nil
	}

	headers := []string{"★", "ID", "Type", "Name", "Code", "Currency"}
	caps := []int{2, 6, 10, 40, 8, 6}
	rows := make([][]string, 0, len(filter))
	for _, j := range filter {
		star := ""
		if favSet[j.ID] {
			star = "★"
		}
		rows = append(rows, []string{
			star,
			strconv.Itoa(j.ID),
			j.Type,
			Truncate(j.Name, caps[3]),
			j.Code,
			j.Currency,
		})
	}
	rightAlign := map[int]bool{1: true}
	renderTable(headers, rows, caps, rightAlign)

	if fromCache {
		fmt.Printf("\n%s(cache: %s — run `odoo pull` to refresh)%s\n", Fmt.Dim, cacheAgeJournals(db), Fmt.Reset)
	}
	fmt.Println()
	return nil
}

func journalsListSubtitle(all bool, search string) string {
	switch {
	case search != "":
		return fmt.Sprintf("search: %q", search)
	case all:
		return "all journals"
	}
	return "favorites only (use --all or --search to widen)"
}

func filterJournals(in []Journal, keyword string) []Journal {
	kw := strings.ToLower(keyword)
	out := make([]Journal, 0, len(in))
	for _, j := range in {
		if strings.Contains(strings.ToLower(j.Name), kw) || strings.Contains(strings.ToLower(j.Code), kw) {
			out = append(out, j)
		}
	}
	return out
}

func filterJournalsByFavorite(in []Journal, fav map[int]bool) []Journal {
	out := make([]Journal, 0, len(fav))
	for _, j := range in {
		if fav[j.ID] {
			out = append(out, j)
		}
	}
	return out
}

// ── detail ──────────────────────────────────────────────────────

func journalDetail(jid int, args []string) error {
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)
	PrintActiveDBBanner(db.Name)

	journals, _, err := loadOrFetchJournals(db)
	if err != nil {
		return err
	}
	var found *Journal
	for i := range journals {
		if journals[i].ID == jid {
			found = &journals[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("journal #%d not in cache (try `odoo pull`)", jid)
	}

	fav, _ := LoadFavorites(db.Name)
	star := ""
	if found.IsFavorite {
		star = " ★"
	}
	if star == "" {
		for _, id := range fav.Journals {
			if id == jid {
				star = " ★"
				break
			}
		}
	}

	fmt.Printf("\n%s▸ %s%s%s%s\n", Fmt.Bold, found.Name, Fmt.Reset, Fmt.Yellow, star)
	fmt.Printf("%s\n", strings.Repeat("─", 60))
	kv := func(k, v string) {
		if v == "" {
			return
		}
		fmt.Printf("  %s%-18s%s %s\n", Fmt.Dim, k, Fmt.Reset, v)
	}
	kv("ID", strconv.Itoa(found.ID))
	kv("Code", found.Code)
	kv("Type", found.Type)
	kv("Currency", found.Currency)
	if found.DefaultAccountID > 0 {
		kv("Default account", strconv.Itoa(found.DefaultAccountID))
	}
	if found.SuspenseAccountID > 0 {
		kv("Suspense account", strconv.Itoa(found.SuspenseAccountID))
	}
	if found.LineCount > 0 {
		kv("Line count", strconv.Itoa(found.LineCount))
	}
	if found.Balance != 0 {
		kv("Balance", FmtAmount(found.Balance, found.Currency))
	}
	kv("Updated", found.Updated)
	kv("Web URL", fmt.Sprintf("%s/odoo/accounting/%d", db.URL, found.ID))

	fmt.Printf("\n  %sActions:%s\n", Fmt.Dim, Fmt.Reset)
	if star == "" {
		fmt.Printf("  %sodoo journals %d favorite%s        Add to favorites\n", Fmt.Cyan, jid, Fmt.Reset)
	} else {
		fmt.Printf("  %sodoo journals %d unfavorite%s      Remove from favorites\n", Fmt.Cyan, jid, Fmt.Reset)
	}
	fmt.Printf("  %sodoo journals %d reconcile -i%s    Interactive reconcile\n\n", Fmt.Cyan, jid, Fmt.Reset)
	return nil
}

// ── favorite / unfavorite ───────────────────────────────────────

func journalFavoriteToggle(jid int, args []string, add bool) error {
	db, err := ResolveActive(args)
	if err != nil {
		return err
	}
	TouchActive(db.Name)
	PrintActiveDBBanner(db.Name)

	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}

	// Write to Odoo first — the cached IsFavorite is a mirror, so
	// Odoo is the source of truth. Which field to write depends on
	// the Odoo version: favorite_user_ids (per-user m2m) on 17+,
	// show_on_dashboard (per-journal bool) on older builds.
	favField := DetectFavoriteField(db, uid)
	if favField == "" {
		return fmt.Errorf("Odoo doesn't expose a favorites field on account.journal (neither favorite_user_ids nor show_on_dashboard) — can't sync this toggle")
	}
	var writeVal interface{}
	switch favField {
	case "favorite_user_ids":
		// Many2many ops: [4, uid] adds, [3, uid] removes.
		op := 4
		if !add {
			op = 3
		}
		writeVal = []interface{}{[]interface{}{op, uid}}
	case "show_on_dashboard":
		writeVal = add
	}
	if _, err := Exec(db.URL, db.DB, uid, db.Password,
		"account.journal", "write",
		[]interface{}{
			[]interface{}{jid},
			map[string]interface{}{favField: writeVal},
		}, nil); err != nil {
		return fmt.Errorf("write account.journal #%d %s: %v", jid, favField, err)
	}
	if favField == "show_on_dashboard" {
		fmt.Printf("  %s↳ wrote show_on_dashboard=%v (this Odoo build has no per-user favorite — change is visible to every user)%s\n",
			Fmt.Dim, add, Fmt.Reset)
	}

	// Patch the cached Journal record so the next `odoo journals`
	// reflects the new state without requiring a full `pull`.
	if file, ok := readJournalsCache(db.Name); ok {
		patched := false
		for i := range file.Journals {
			if file.Journals[i].ID == jid {
				file.Journals[i].IsFavorite = add
				patched = true
				break
			}
		}
		if patched {
			_ = WriteJournalsCache(db.Name, file.Journals)
		}
	}

	// Clean any leftover entry from the legacy local favorites file
	// so the two systems can't drift. Adding via this path is no
	// longer needed (cached IsFavorite is authoritative), but we
	// keep the file alive in case a pre-upgrade install populated it.
	fav, _ := LoadFavorites(db.Name)
	out := fav.Journals[:0]
	for _, id := range fav.Journals {
		if id != jid {
			out = append(out, id)
		}
	}
	if len(out) != len(fav.Journals) {
		fav.Journals = out
		_ = SaveFavorites(db.Name, fav)
	}

	verb := "added to"
	if !add {
		verb = "removed from"
	}
	fmt.Printf("\n%s✓ Journal #%d %s favorites on %s%s\n\n",
		Fmt.Green, jid, verb, db.Host(), Fmt.Reset)
	return nil
}

// journalReconcileStub used to defer reconcile to a follow-up
// commit; it now routes to the canonical implementation. Kept under
// the old name (referenced from the journals dispatch) to minimise
// churn — the actual logic lives in journals_reconcile.go.
func journalReconcileStub(jid int, args []string) error {
	return Reconcile(jid, args)
}

// ── fetch + cache ───────────────────────────────────────────────

// loadOrFetchJournals returns the cached journal list when present;
// otherwise fetches from Odoo on the fly, writes the cache, and
// returns. fromCache is true when the result came from disk.
func loadOrFetchJournals(db *Database) (journals []Journal, fromCache bool, err error) {
	if j, ok := readJournalsCache(db.Name); ok && len(j.Journals) > 0 {
		return j.Journals, true, nil
	}
	// No cache → fetch on the fly.
	fmt.Printf("%s● Fetching journals from %s …%s\n", Fmt.Dim, db.URL, Fmt.Reset)
	uid, err := AuthDatabase(db)
	if err != nil {
		return nil, false, err
	}
	fetched, err := FetchJournals(db, uid)
	if err != nil {
		return nil, false, err
	}
	if err := WriteJournalsCache(db.Name, fetched); err != nil {
		// Non-fatal: cache write failure shouldn't block the read.
		fmt.Fprintf(os.Stderr, "  %s⚠ could not write journals cache: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
	}
	return fetched, false, nil
}

// FetchJournals reads every account.journal from Odoo, returning
// the slim Journal shape this CLI cares about. IsFavorite mirrors
// whichever favorite mechanism the Odoo version exposes:
//
//   - Odoo 17+ : account.journal.favorite_user_ids (per-user m2m)
//   - Odoo ≤16: account.journal.show_on_dashboard  (per-journal bool)
//
// Detection happens once at the top via DetectFavoriteField; the
// chosen field is also what `odoo journals <id> favorite/unfavorite`
// writes to.
func FetchJournals(db *Database, uid int) ([]Journal, error) {
	favField := DetectFavoriteField(db, uid)
	fields := []string{"id", "name", "code", "type", "currency_id",
		"default_account_id", "suspense_account_id", "active"}
	if favField != "" {
		fields = append(fields, favField)
	}
	rows, err := SearchReadAllMaps(db, uid, "account.journal",
		[]interface{}{}, fields, "name asc")
	if err != nil {
		return nil, err
	}
	out := make([]Journal, 0, len(rows))
	now := time.Now().UTC().Format(time.RFC3339)
	for _, r := range rows {
		isFav := false
		switch favField {
		case "favorite_user_ids":
			isFav = isFavoriteForUID(r["favorite_user_ids"], uid)
		case "show_on_dashboard":
			isFav = Bool(r["show_on_dashboard"])
		}
		out = append(out, Journal{
			ID:                Int(r["id"]),
			Name:              Str(r["name"]),
			Code:              Str(r["code"]),
			Type:              Str(r["type"]),
			Currency:          FieldName(r["currency_id"]),
			DefaultAccountID:  FieldID(r["default_account_id"]),
			SuspenseAccountID: FieldID(r["suspense_account_id"]),
			Active:            Bool(r["active"]),
			IsFavorite:        isFav,
			Updated:           now,
		})
	}
	return out, nil
}

// DetectFavoriteField returns the name of the field this Odoo
// instance uses to model "favorite" journals. Calls fields_get once
// (cheap) and prefers favorite_user_ids over show_on_dashboard when
// both exist, since favorite_user_ids is per-user (more accurate).
// Returns "" when neither field exists — caller treats every journal
// as non-favorite.
func DetectFavoriteField(db *Database, uid int) string {
	raw, err := Exec(db.URL, db.DB, uid, db.Password,
		"account.journal", "fields_get",
		[]interface{}{[]interface{}{"favorite_user_ids", "show_on_dashboard"}},
		map[string]interface{}{"attributes": []interface{}{"type"}})
	if err != nil {
		return ""
	}
	var result map[string]interface{}
	if jerr := json.Unmarshal(raw, &result); jerr != nil {
		return ""
	}
	if _, ok := result["favorite_user_ids"]; ok {
		return "favorite_user_ids"
	}
	if _, ok := result["show_on_dashboard"]; ok {
		return "show_on_dashboard"
	}
	return ""
}

// isFavoriteForUID reports whether uid appears in the many2many
// favorite_user_ids array returned by search_read.
func isFavoriteForUID(v interface{}, uid int) bool {
	arr, ok := v.([]interface{})
	if !ok {
		return false
	}
	for _, x := range arr {
		if Int(x) == uid {
			return true
		}
	}
	return false
}

// diagnoseFavorites prints, per journal, whichever favorite field
// Odoo exposes on this instance. Used by `odoo pull -v` to debug
// situations where the operator has favorites starred on the Odoo
// dashboard but the CLI doesn't see them.
func diagnoseFavorites(db *Database, uid int, cached []Journal) {
	favField := DetectFavoriteField(db, uid)
	fmt.Printf("    %s── favorites diagnostic (uid=%d, field=%s) ──%s\n",
		Fmt.Dim, uid, defaultIfEmpty(favField, "<none>"), Fmt.Reset)
	if favField == "" {
		fmt.Fprintf(os.Stderr, "    %s⚠ Odoo exposes neither favorite_user_ids nor show_on_dashboard on account.journal — no favorites can sync.%s\n",
			Fmt.Yellow, Fmt.Reset)
		return
	}
	rows, err := SearchReadAllMaps(db, uid, "account.journal",
		[]interface{}{},
		[]string{"id", "name", "code", favField},
		"name asc",
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "    %s⚠ favorites diagnostic failed: %v%s\n", Fmt.Yellow, err, Fmt.Reset)
		return
	}
	favCount := 0
	for _, r := range rows {
		fav := false
		switch favField {
		case "favorite_user_ids":
			fav = isFavoriteForUID(r[favField], uid)
		case "show_on_dashboard":
			fav = Bool(r[favField])
		}
		if fav {
			favCount++
		}
		marker := " "
		if fav {
			marker = "★"
		}
		fmt.Printf("    %s%s #%-4d %-32s %s=%v%s\n",
			Fmt.Dim, marker, Int(r["id"]), Truncate(Str(r["name"]), 32),
			favField, r[favField], Fmt.Reset)
	}
	fmt.Printf("    %ssummary: %d journals flagged via %s%s\n",
		Fmt.Dim, favCount, favField, Fmt.Reset)
	_ = cached
}

// WriteJournalsCache persists the journal list under
// ~/.odoo/cache/<dbname>/journals/list.json.
func WriteJournalsCache(dbname string, journals []Journal) error {
	if err := EnsureCacheDirs(dbname); err != nil {
		return err
	}
	file := JournalsListFile{
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Count:     len(journals),
		Journals:  journals,
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(JournalsCacheDir(dbname), "list.json"), data, 0600)
}

func readJournalsCache(dbname string) (JournalsListFile, bool) {
	data, err := os.ReadFile(filepath.Join(JournalsCacheDir(dbname), "list.json"))
	if err != nil {
		return JournalsListFile{}, false
	}
	var file JournalsListFile
	if err := json.Unmarshal(data, &file); err != nil {
		return JournalsListFile{}, false
	}
	return file, true
}

func cacheAgeJournals(db *Database) string {
	file, ok := readJournalsCache(db.Name)
	if !ok || file.FetchedAt == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339, file.FetchedAt)
	if err != nil {
		return file.FetchedAt
	}
	return humanAgo(t)
}

// LoadFavorites reads favorites.json. Empty Favorites on first run.
func LoadFavorites(dbname string) (*Favorites, error) {
	data, err := os.ReadFile(FavoritesPath(dbname))
	if err != nil {
		if os.IsNotExist(err) {
			return &Favorites{}, nil
		}
		return &Favorites{}, err
	}
	var f Favorites
	if err := json.Unmarshal(data, &f); err != nil {
		return &Favorites{}, err
	}
	return &f, nil
}

// SaveFavorites writes favorites.json (0600).
func SaveFavorites(dbname string, f *Favorites) error {
	if err := EnsureCacheDirs(dbname); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(FavoritesPath(dbname), data, 0600)
}

// resolveJournalByCode returns the journal id for a given code,
// using the cache. 0 if not found.
func resolveJournalByCode(args []string, code string) int {
	state, _ := LoadState()
	dbname := GetOption(args, "--db")
	if dbname == "" {
		dbname = state.ActiveDB
	}
	if dbname == "" {
		return 0
	}
	file, ok := readJournalsCache(dbname)
	if !ok {
		return 0
	}
	for _, j := range file.Journals {
		if strings.EqualFold(j.Code, code) {
			return j.ID
		}
	}
	return 0
}

// ── helpers ────────────────────────────────────────────────────

func nthPositional(args []string, n int, valueFlags ...string) string {
	count := 0
	skip := false
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
		count++
		if count == n {
			return a
		}
	}
	return ""
}

func isFlag(s string) bool {
	return len(s) > 0 && s[0] == '-'
}

// renderTable prints a simple table with rune-width-aware columns.
// Caps are applied as truncation limits per column.
func renderTable(headers []string, rows [][]string, caps []int, rightAlign map[int]bool) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = DisplayWidth(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if w := DisplayWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	for i := range widths {
		if widths[i] > caps[i] {
			widths[i] = caps[i]
		}
	}
	render := func(cells []string, dim bool) {
		fmt.Print("  ")
		for i, c := range cells {
			if i > 0 {
				fmt.Print("  ")
			}
			if rightAlign[i] {
				c = PadLeft(c, widths[i])
			} else {
				c = PadRight(c, widths[i])
			}
			if dim {
				c = Fmt.Dim + c + Fmt.Reset
			}
			fmt.Print(c)
		}
		fmt.Println()
	}
	render(headers, true)
	for _, r := range rows {
		render(r, false)
	}
}

// Pluralize returns either singular or plural based on n. Plural is
// auto-derived as singular+"s" when no explicit plural is provided.
func Pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	if plural == "" {
		return singular + "s"
	}
	return plural
}

func printJournalsHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo journals%s — list / inspect / favorite Odoo journals

%sUSAGE%s
  %sodoo journals%s                       List favorite journals
  %sodoo journals%s --all                 List every journal
  %sodoo journals%s --search KW           Substring match on name / code
  %sodoo journals%s <id>                  Show one journal's detail
  %sodoo journals%s <id> favorite         Mark as favorite
  %sodoo journals%s <id> unfavorite       Remove from favorites
  %sodoo journals%s <id> reconcile [-i] [--yes]
                                     Reconcile unmatched bank lines (TUI with -i)
  %sodoo journals%s <id> unreconcile --account <code|id> [--yes]
                                     Unlink every reconciliation on a journal+account

%sBEHAVIOUR%s
  Reads from ~/.odoo/cache/<dbname>/journals/list.json when present;
  otherwise fetches from Odoo on the fly and writes the cache. Run
  %sodoo pull%s to force a refresh.

  Favorites are stored per-database at ~/.odoo/cache/<dbname>/favorites.json.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
	)
}
