package cmd

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Reconcile is `odoo journals <id> reconcile [-i] [--yes]`. Two
// modes:
//
//	non-interactive (default): walk every unreconciled bank line in
//	  the journal, find its Suggestion, act on the unambiguous
//	  unreconciled match (when --yes is passed) or print a dry-run
//	  preview. AlreadyAttached candidates are filtered out — only
//	  the interactive picker can override an existing reconciliation.
//
//	interactive (-i): bubbletea TUI. Walks lines newest-first;
//	  per line shows the Suggestion picker (Status badge for
//	  AlreadyAttached candidates). Enter on a fresh candidate
//	  attaches; Enter on an AlreadyAttached candidate prompts for
//	  [y] before triggering unreconcile + reattach. Esc/q backs
//	  out without applying.
func Reconcile(jid int, args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printReconcileHelp()
		return nil
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

	cache := loadJournalLines(db.Name, jid)
	if cache == nil {
		return fmt.Errorf("journal #%d has no cached lines — run `odoo pull` first", jid)
	}
	unreconciled := make([]JournalLine, 0, len(cache.Lines))
	for _, ln := range cache.Lines {
		if !ln.IsReconciled && ln.Amount != 0 {
			unreconciled = append(unreconciled, ln)
		}
	}
	if len(unreconciled) == 0 {
		fmt.Printf("\n%sJournal #%d has no unreconciled bank lines — nothing to do.%s\n\n", Fmt.Dim, jid, Fmt.Reset)
		return nil
	}
	// Newest first matches the TUI's walk order so the operator's
	// freshest activity is most prominent.
	sort.SliceStable(unreconciled, func(i, j int) bool { return unreconciled[i].Date > unreconciled[j].Date })

	if interactive {
		return reconcileInteractive(db, jid, unreconciled)
	}
	return reconcileBatch(db, jid, unreconciled, assumeYes, dryRun, verbose)
}

// ── non-interactive batch ───────────────────────────────────────

type reconcilePlan struct {
	Line     JournalLine
	Suggs    []Suggestion
	Decision string // "match" / "ambiguous" / "none"
}

func reconcileBatch(db *Database, jid int, lines []JournalLine, assumeYes, dryRun, verbose bool) error {
	plans := make([]reconcilePlan, 0, len(lines))
	var matched, ambiguous, none int
	for _, ln := range lines {
		all := SuggestForBankLine(db.Name, ln)
		open := make([]Suggestion, 0, len(all))
		for _, s := range all {
			if !s.AlreadyAttached {
				open = append(open, s)
			}
		}
		p := reconcilePlan{Line: ln, Suggs: open}
		switch {
		case len(open) == 0:
			p.Decision = "none"
			none++
		case len(open) == 1:
			p.Decision = "match"
			matched++
		default:
			p.Decision = "ambiguous"
			ambiguous++
		}
		plans = append(plans, p)
	}

	fmt.Printf("\n%sReconcile journal #%d — %d unreconciled line%s%s\n",
		Fmt.Bold, jid, len(lines), pluralS(len(lines)), Fmt.Reset)
	fmt.Printf("  %sCandidates  matched: %d · ambiguous: %d · no-match: %d%s\n\n",
		Fmt.Dim, matched, ambiguous, none, Fmt.Reset)

	if verbose {
		for _, p := range plans {
			printReconcilePlanRow(p, true)
		}
	} else if matched > 0 {
		for _, p := range plans {
			if p.Decision == "match" {
				printReconcilePlanRow(p, false)
			}
		}
	}
	if matched == 0 {
		fmt.Printf("\n%sNothing to reconcile.%s Re-run with %s-i%s to resolve ambiguous lines interactively.\n\n",
			Fmt.Dim, Fmt.Reset, Fmt.Cyan, Fmt.Reset)
		return nil
	}

	if dryRun {
		fmt.Printf("\n%s(dry-run — re-run with --yes to apply.)%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}
	if !assumeYes && isTTY() {
		fmt.Printf("\n%sReconcile %d match%s on Odoo at %s?%s [Y/n] ",
			Fmt.Bold, matched, pluralS(matched), db.Host(), Fmt.Reset)
		resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		resp = strings.ToLower(strings.TrimSpace(resp))
		if resp == "n" || resp == "no" {
			fmt.Println("  cancelled.")
			return nil
		}
	} else if !assumeYes {
		return fmt.Errorf("refusing to write on a non-TTY without --yes")
	}

	uid, err := AuthDatabase(db)
	if err != nil {
		return err
	}
	var applied, failed int
	for _, p := range plans {
		if p.Decision != "match" {
			continue
		}
		sugg := p.Suggs[0]
		if err := ReconcileBankLineWithInvoice(db, uid, p.Line, sugg.Move); err != nil {
			failed++
			fmt.Printf("  %s✗%s line #%d → %s #%d: %v\n", Fmt.Red, Fmt.Reset, p.Line.ID, sugg.Move.MoveType, sugg.Move.ID, err)
			continue
		}
		applied++
		if verbose {
			fmt.Printf("  %s✓%s line #%d → %s %s (%s)\n",
				Fmt.Green, Fmt.Reset, p.Line.ID, sugg.Move.MoveType, sugg.Move.Name, sugg.Move.PartnerName)
		}
	}
	fmt.Printf("\n%sReconciled %d match%s%s", Fmt.Green, applied, pluralS(applied), Fmt.Reset)
	if failed > 0 {
		fmt.Printf("  %s(%d failed)%s", Fmt.Red, failed, Fmt.Reset)
	}
	fmt.Println()
	// Update last-sync push timestamp.
	if last := LoadLastSync(db.Name); last != nil {
		last.PushedAt = time.Now().UTC().Format(time.RFC3339)
		_ = writeLastSync(db.Name, last)
	}
	fmt.Println()
	return nil
}

func printReconcilePlanRow(p reconcilePlan, verbose bool) {
	icon, color := "?", Fmt.Yellow
	switch p.Decision {
	case "match":
		icon, color = "✓", Fmt.Green
	case "none":
		icon, color = "·", Fmt.Dim
	}
	fmt.Printf("  %s%s%s %s  %12s  %s\n",
		color, icon, Fmt.Reset,
		p.Line.Date, FmtAmount(p.Line.Amount, "EUR"),
		Truncate(FirstNonEmpty(p.Line.PaymentRef, p.Line.Narration), 50))
	switch p.Decision {
	case "match":
		s := p.Suggs[0]
		fmt.Printf("      %s→%s %s %s · %s · %s\n",
			Fmt.Dim, Fmt.Reset,
			s.Move.MoveType, s.Move.Name,
			Truncate(s.Partner, 32), s.Date)
	case "ambiguous":
		fmt.Printf("      %s? %d open candidate(s) — pass -i to resolve%s\n", Fmt.Dim, len(p.Suggs), Fmt.Reset)
		if verbose {
			limit := len(p.Suggs)
			if limit > 3 {
				limit = 3
			}
			for i := 0; i < limit; i++ {
				s := p.Suggs[i]
				fmt.Printf("        %s· %s %s · %s · %s%s\n",
					Fmt.Dim, s.Move.MoveType, s.Move.Name,
					Truncate(s.Partner, 32), s.Date, Fmt.Reset)
			}
		}
	}
}

// ── interactive TUI ─────────────────────────────────────────────

type reconcileTUIMode int

const (
	reconcileModeWalk reconcileTUIMode = iota
	reconcileModePick
)

type reconcileTUIModel struct {
	db       *Database
	jid      int
	lines    []JournalLine
	lineIdx  int

	// Per-line picker state, populated lazily when the operator
	// enters the pick mode for the current line.
	mode             reconcileTUIMode
	suggestions      []Suggestion
	suggCursor       int
	confirmReattach  bool

	// Outcomes — one slot per input line, populated as the
	// operator works through them.
	outcomes []string // "" / "attached" / "skipped" / "no-match"

	status      string
	statusError bool
	uid         int
	uidReady    bool
	width       int
	height      int

	// Help search overlay (filter the suggestions by name) —
	// future extension; not yet implemented.
	_search textinput.Model
}

var (
	reconHeader = lipgloss.NewStyle().Bold(true)
	reconDim    = lipgloss.NewStyle().Faint(true)
	reconCursor = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("255"))
	reconOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	reconErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	reconWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

func reconcileInteractive(db *Database, jid int, lines []JournalLine) error {
	m := &reconcileTUIModel{
		db:       db,
		jid:      jid,
		lines:    lines,
		outcomes: make([]string, len(lines)),
	}
	prog := tea.NewProgram(m, tea.WithAltScreen())
	_, err := prog.Run()
	return err
}

func (m *reconcileTUIModel) Init() tea.Cmd { return nil }

func (m *reconcileTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch m.mode {
		case reconcileModePick:
			return m.updatePick(msg)
		}
		return m.updateWalk(msg)
	}
	return m, nil
}

func (m *reconcileTUIModel) updateWalk(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.lineIdx > 0 {
			m.lineIdx--
		}
	case "down", "j":
		if m.lineIdx < len(m.lines)-1 {
			m.lineIdx++
		}
	case "s":
		// Skip this line (mark as "skipped" and advance).
		if m.lineIdx < len(m.outcomes) {
			m.outcomes[m.lineIdx] = "skipped"
		}
		if m.lineIdx < len(m.lines)-1 {
			m.lineIdx++
		}
	case "enter":
		if m.lineIdx < 0 || m.lineIdx >= len(m.lines) {
			return m, nil
		}
		m.suggestions = SuggestForBankLine(m.db.Name, m.lines[m.lineIdx])
		if len(m.suggestions) == 0 {
			m.outcomes[m.lineIdx] = "no-match"
			m.status = fmt.Sprintf("No matching invoice/bill found for line #%d (amount %s) — checked open AND paid.",
				m.lines[m.lineIdx].ID, FmtAmount(m.lines[m.lineIdx].Amount, "EUR"))
			m.statusError = false
			return m, nil
		}
		m.mode = reconcileModePick
		m.suggCursor = FirstUnattachedIndex(m.suggestions)
		m.confirmReattach = false
		m.status = ""
		m.statusError = false
	}
	return m, nil
}

func (m *reconcileTUIModel) updatePick(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = reconcileModeWalk
		m.suggestions = nil
		m.suggCursor = 0
		m.confirmReattach = false
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.confirmReattach = false
		if m.suggCursor > 0 {
			m.suggCursor--
		}
		return m, nil
	case "down", "j":
		m.confirmReattach = false
		if m.suggCursor < len(m.suggestions)-1 {
			m.suggCursor++
		}
		return m, nil
	case "enter":
		if m.suggCursor < 0 || m.suggCursor >= len(m.suggestions) {
			return m, nil
		}
		sugg := m.suggestions[m.suggCursor]
		if sugg.AlreadyAttached && !m.confirmReattach {
			m.confirmReattach = true
			m.status = fmt.Sprintf("↻ %s %s is already %s. Press [y] to UNRECONCILE its existing match and reattach this bank line, or [esc] to back out.",
				sugg.Move.MoveType, sugg.Move.Name,
				defaultIfEmpty(sugg.PaymentState, "settled"))
			m.statusError = false
			return m, nil
		}
		return m, m.applyOrPrompt(sugg)
	case "y", "Y":
		if !m.confirmReattach || m.suggCursor < 0 || m.suggCursor >= len(m.suggestions) {
			return m, nil
		}
		return m, m.applyOrPrompt(m.suggestions[m.suggCursor])
	}
	return m, nil
}

// applyOrPrompt is the bridge between the picker's Enter/y and the
// actual Odoo write. Calls AuthDatabase once (cached on the model)
// then ReconcileBankLineWithInvoice. Updates outcomes + status.
// Returns a tea.Cmd that's always nil; using the method form keeps
// the Update calls regular.
func (m *reconcileTUIModel) applyOrPrompt(sugg Suggestion) tea.Cmd {
	line := m.lines[m.lineIdx]
	if !m.uidReady {
		uid, err := AuthDatabase(m.db)
		if err != nil {
			m.status = fmt.Sprintf("Auth failed: %v", err)
			m.statusError = true
			m.confirmReattach = false
			return nil
		}
		m.uid = uid
		m.uidReady = true
	}
	if err := ReconcileBankLineWithInvoice(m.db, m.uid, line, sugg.Move); err != nil {
		m.status = fmt.Sprintf("Reconcile failed: %v", err)
		m.statusError = true
		m.confirmReattach = false
		return nil
	}
	verb := "Attached"
	if sugg.AlreadyAttached {
		verb = "Reattached (unreconciled + relinked)"
	}
	m.status = fmt.Sprintf("✓ %s line #%d → %s %s (%s)",
		verb, line.ID, sugg.Move.MoveType, sugg.Move.Name, sugg.Move.PartnerName)
	m.statusError = false
	m.outcomes[m.lineIdx] = "attached"
	m.mode = reconcileModeWalk
	m.suggestions = nil
	m.suggCursor = 0
	m.confirmReattach = false
	// Auto-advance to the next un-resolved line.
	for i := m.lineIdx + 1; i < len(m.lines); i++ {
		if m.outcomes[i] == "" {
			m.lineIdx = i
			break
		}
	}
	return nil
}

func (m *reconcileTUIModel) View() string {
	if m.mode == reconcileModePick {
		return m.viewPick()
	}
	return m.viewWalk()
}

func (m *reconcileTUIModel) viewWalk() string {
	var b strings.Builder
	doneCount := 0
	for _, o := range m.outcomes {
		if o != "" {
			doneCount++
		}
	}
	b.WriteString(reconHeader.Render(fmt.Sprintf("⇄ Reconcile journal #%d — %d/%d resolved", m.jid, doneCount, len(m.lines))))
	b.WriteString("\n")
	b.WriteString(reconDim.Render(fmt.Sprintf("db: %s · %s", m.db.Name, m.db.Host())))
	b.WriteString("\n\n")

	pageSize := m.height - 8
	if pageSize < 6 {
		pageSize = 20
	}
	start := m.lineIdx - pageSize/2
	if start < 0 {
		start = 0
	}
	end := start + pageSize
	if end > len(m.lines) {
		end = len(m.lines)
	}

	headers := []string{"Status", "Date", "Amount", "Partner", "Description"}
	caps := []int{8, 10, 14, 22, 50}
	rows := make([][]string, 0, end-start)
	for i := start; i < end; i++ {
		ln := m.lines[i]
		marker := " "
		if i == m.lineIdx {
			marker = "▸"
		}
		status := defaultIfEmpty(m.outcomes[i], "pending")
		partner := ""
		if ln.PartnerID > 0 {
			idx := loadPartnersIndex(m.db.Name)
			if idx != nil {
				if p, ok := idx.ByID[ln.PartnerID]; ok && p != nil {
					partner = p.Name
				}
			}
		}
		rows = append(rows, []string{
			marker + " " + status,
			ln.Date,
			FmtAmount(ln.Amount, "EUR"),
			Truncate(partner, caps[3]),
			Truncate(FirstNonEmpty(ln.PaymentRef, ln.Narration), caps[4]),
		})
	}
	renderTUITable(&b, headers, rows, caps, map[int]bool{2: true}, m.lineIdx-start)

	if m.status != "" {
		b.WriteString("\n  ")
		if m.statusError {
			b.WriteString(reconErr.Render(m.status))
		} else {
			b.WriteString(reconOK.Render(m.status))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n  ")
	b.WriteString(reconDim.Render("[↑/↓] navigate  [enter] pick candidate  [s] skip  [q] quit"))
	b.WriteString("\n")
	return b.String()
}

func (m *reconcileTUIModel) viewPick() string {
	var b strings.Builder
	line := m.lines[m.lineIdx]
	b.WriteString(reconHeader.Render(fmt.Sprintf("⇄ Pick candidate for line #%d (%s, %s)",
		line.ID, line.Date, FmtAmount(line.Amount, "EUR"))))
	b.WriteString("\n")
	openHits, attachedHits := 0, 0
	for _, s := range m.suggestions {
		if s.AlreadyAttached {
			attachedHits++
		} else {
			openHits++
		}
	}
	subtitle := fmt.Sprintf("  %d candidate(s) — %d open · %d already paid (pick to unreconcile+reattach)",
		len(m.suggestions), openHits, attachedHits)
	b.WriteString(reconDim.Render(subtitle))
	b.WriteString("\n\n")

	headers := []string{"Sel", "Status", "Partner-match", "Date", "Δ", "Number", "Partner", "First line"}
	caps := []int{3, 8, 5, 10, 6, 18, 22, 36}
	rows := make([][]string, 0, len(m.suggestions))
	for i, s := range m.suggestions {
		marker := " "
		if i == m.suggCursor {
			marker = "▸"
		}
		status := ""
		if s.AlreadyAttached {
			status = defaultIfEmpty(strings.ReplaceAll(s.PaymentState, "_", " "), "paid")
		}
		match := ""
		if s.PartnerMatch {
			match = "★"
		}
		rows = append(rows, []string{
			marker,
			status,
			match,
			s.Date,
			fmt.Sprintf("%dd", s.DaysDelta),
			Truncate(s.Move.Name, caps[5]),
			Truncate(s.Partner, caps[6]),
			Truncate(s.Description, caps[7]),
		})
	}
	renderTUITable(&b, headers, rows, caps, map[int]bool{}, m.suggCursor)

	// Detail panel for the cursor candidate.
	if m.suggCursor >= 0 && m.suggCursor < len(m.suggestions) {
		s := m.suggestions[m.suggCursor]
		b.WriteString("\n  ")
		b.WriteString(reconHeader.Render("▸ " + FirstNonEmpty(s.Partner, s.Move.Name)))
		b.WriteString("\n")
		kv := func(k, v string) {
			if v == "" {
				return
			}
			b.WriteString("  ")
			b.WriteString(reconDim.Render(PadRight(k, 14)))
			b.WriteString(v)
			b.WriteString("\n")
		}
		kv("Type", s.Move.MoveType)
		kv("Number", s.Move.Name)
		kv("Date", s.Date)
		kv("Residual", FmtAmount(s.Amount, "EUR"))
		kv("Partner", s.Partner)
		if s.AlreadyAttached {
			kv("State", defaultIfEmpty(s.PaymentState, "settled")+" — reattach will unreconcile the existing match")
		}
		kv("First line", s.Description)
	}

	if m.status != "" {
		b.WriteString("\n  ")
		switch {
		case m.statusError:
			b.WriteString(reconErr.Render(m.status))
		case m.confirmReattach:
			b.WriteString(reconWarn.Render(m.status))
		default:
			b.WriteString(reconOK.Render(m.status))
		}
		b.WriteString("\n")
	}

	hint := "[↑/↓] pick  [enter] attach (writes to Odoo)  [esc] back"
	if m.confirmReattach {
		hint = "[y] confirm unreconcile+reattach  [esc] cancel"
	}
	b.WriteString("\n  ")
	b.WriteString(reconDim.Render(hint))
	b.WriteString("\n")
	return b.String()
}

// renderTUITable is a small reusable renderer for the model views.
func renderTUITable(b *strings.Builder, headers []string, rows [][]string, caps []int, rightAlign map[int]bool, cursorIdx int) {
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
	renderRow := func(cells []string, isHeader, isCursor bool) string {
		parts := make([]string, len(cells))
		for i, c := range cells {
			if rightAlign[i] {
				parts[i] = PadLeft(c, widths[i])
			} else {
				parts[i] = PadRight(c, widths[i])
			}
		}
		line := "  " + strings.Join(parts, "  ")
		switch {
		case isHeader:
			return reconDim.Render(line)
		case isCursor:
			return reconCursor.Render(line)
		}
		return line
	}
	b.WriteString(renderRow(headers, true, false))
	b.WriteString("\n")
	for i, r := range rows {
		b.WriteString(renderRow(r, false, i == cursorIdx))
		b.WriteString("\n")
	}
}

func defaultIfEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func printReconcileHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo journals <id> reconcile%s — match unreconciled bank lines with invoices/bills

%sUSAGE%s
  %sodoo journals 47 reconcile%s              Dry-run preview (unambiguous matches only)
  %sodoo journals 47 reconcile --yes%s        Apply unambiguous matches in batch
  %sodoo journals 47 reconcile -i%s           Open the TUI: walk lines, pick candidates
  %sodoo journals 47 reconcile -i -v%s        TUI + verbose progress

%sBEHAVIOUR%s
  The matcher walks every unreconciled bank statement line on the
  journal, computes a Suggestion list per line (amount + direction +
  partner fuzzy + date proximity, two-tier ordering), and:

    - Non-interactive (default): acts on lines that have exactly ONE
      OPEN invoice/bill candidate. Lines with no match, ambiguous
      matches, or only-already-paid matches are reported but not
      written. Default is dry-run; --yes applies after a Y/n prompt.
    - Interactive (-i): TUI walks lines newest-first. Enter on a
      line opens its candidate picker; Enter on a candidate attaches.
      If the candidate is already paid, the picker requires [y] to
      confirm unreconcile + reattach.

  Writes use the same Odoo dance the bank-reconciliation widget
  does: draft → rewrite the suspense counterpart's account_id to
  the invoice's A/R account → repost → reconcile.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
	)
}
