# CLAUDE.md — guidance for Claude working on odoo-cli

This is a sibling project of [chb](https://github.com/CommonsHub/chb).
Patterns are ported from chb, scope is narrower: Odoo-only, no
providers, no generated files. Read chb's `CLAUDE.md` and
`docs/philosophy.md` first if you haven't already.

## The hard rule

- **`pull` only fetches from Odoo into `~/.odoo/cache/<dbname>/`.
  No transformation, no normalisation. Re-running `pull` must
  never invent records that didn't come from Odoo.**
- **Local edits become files in `pending/`. `push` flushes them
  to Odoo and archives the file to `sent/`.**
- **`sync` = `pull` + `push`.** Nothing else.

If a presentation bug surfaces in the journal-list or detail view,
the fix lives in the read path that renders it — never in `pull`.

## Layout

One Go package under `cmd/`:

```
cmd/
  colors.go        Fmt struct (auto-off for non-TTY / NO_COLOR)
  flags.go         HasFlag / GetOption / RemoveOption / FirstPositional
  format.go        Truncate / Pad{Left,Right} / DisplayWidth / FmtEUR / Pluralize
  help.go          PrintTopHelp + PrintActiveDBBanner
  paths.go         AppRoot / DatabasesDir / CacheDir / PendingDir / etc.
  state.go         ~/.odoo/state.json loader + SetActiveDB / TouchActive
  database.go      ~/.odoo/databases/*.env loader + ResolveActive
  rpc.go           XML-RPC layer (RPC / Auth / Exec / SearchReadAllMaps)
  setup.go         odoo setup
  switch.go        odoo switch
  journals.go      odoo journals (list / detail / favorite / unfavorite)
  pull.go          odoo pull
  pending.go       outbox helpers
  push.go          odoo push
  sync.go          odoo sync
  reconcile.go     odoo journals <id> reconcile (TBD)
```

`main.go` is the dispatcher; everything else lives in `cmd/`.

## Conventions

- **Every command prints the active DB at the top** via
  `cmd.PrintActiveDBBanner(db.Name)`. Commands that don't need a DB
  (setup, switch, --help) skip it.
- **`--help` short-circuits at the top of every command**. No
  further work, no banner, just print and return. Never strip
  `--help` mid-pipeline — it must be the first thing checked.
- **Non-interactive by default.** Sub-commands open a TUI only
  when `-i` / `--interactive` is passed.
- **Destructive ops require `--yes` OR a TTY confirm**. Non-TTY
  without `--yes` refuses.
- **`--dry-run`** previews; `--yes` applies; pair them for
  forced dry-run that ignores `--yes` (chb pattern).
- **Compact by default**; `-v` / `--verbose` for per-row detail.
- **`Fmt` struct** for colours. No hard-coded ANSI escapes.
- **`DisplayWidth(s)` = rune count.** Tables use it for alignment
  so unicode (★, ✓, →, etc.) doesn't break columns.

## Multi-database

- `--db <name>` is a global flag — overrides the active DB for one
  invocation. The dispatcher in `main.go` doesn't strip it; sub-
  commands pass `args` straight through to `ResolveActive(args)`.
- The active DB persists in `~/.odoo/state.json` and is updated
  by `odoo switch` / `odoo setup`. `TouchActive(name)` bumps the
  last-used timestamp at the start of every command that reads
  from a DB.
- **Every command must call `ResolveActive(args)` before doing
  anything DB-related.** It returns a friendly "run `odoo setup`"
  or "run `odoo switch`" hint when nothing resolves.

## Pending changes

- Each pending mutation is one JSON file under
  `~/.odoo/cache/<dbname>/pending/<id>.json`.
- `id` = `<unix-nanos>-<sanitised-kind>` (sortable, deduplicable,
  human-readable).
- Shape: `PendingChange{ID, Kind, Payload, CreatedAt, LastError, Attempts}`.
- `Push` dispatches by `Kind` via a switch in `applyPending`.
  New mutation types register here.
- Successful changes MOVE (rename, not copy) to `sent/`. Failures
  stamp `LastError` and stay in `pending/` for the next push to
  retry. Never block the next change on a previous failure.

## RPC etiquette

- All Odoo writes go through `cmd.Exec`. Reads through
  `cmd.SearchReadAllMaps`.
- Default 200-row pagination. Don't hand-roll pagination.
- HTTP 429 is handled automatically (Retry-After or exponential
  backoff). Other transport errors are mapped to friendly messages
  in `friendlyTransportError` / `friendlyRPCError`. Add to those
  when a new failure shape needs human-readable handling.
- `AuthDatabase(db)` is the canonical entrypoint — every command
  that touches Odoo calls it once at the top of its flow.

## Reconcile (future)

The big port — covers the same ground as chb's
`reconcileStatementLineWithMove`, `findInvoicePaymentCandidates`,
`findInvoiceCandidatesForTx`, and the interactive resolver:

- Local matcher: amount + direction + partner fuzzy + date proximity.
- Two-pass widening: unreconciled first; on empty, broaden to all
  posted candidates with an "already paid" badge — picking one
  triggers unreconcile + reattach.
- Apply path: draft → rewrite suspense counterpart → repost →
  reconcile. The `withOdooMoveTemporarilyDraft` lifecycle helper
  needs porting too.
- TUI: bubbletea picker; up/down to navigate, Enter to attach,
  `[esc]` back. Mirror chb's `invoices_tui.go`'s `[r]` overlay
  shape.
- Non-interactive batch: only act on unambiguous unreconciled
  matches; surface ambiguous + no-match counts.

Don't widen scope here. Start with the matcher and the
non-interactive batch; layer the TUI after.

## Tests

- Unit tests live next to the file they test.
- Use `ODOO_CLI_ROOT=/tmp/...` to keep tests off the user's
  real `~/.odoo`.
- Don't add a real-Odoo dependency to the test path. Stub the
  RPC layer behind a small interface if you need to test it.

## Don't

- Don't import the chb package. We ported helpers by hand; keep
  it that way.
- Don't add a `generate` phase. odoo-cli's pipeline is `pull` →
  cache → (operator decides via TUI) → `pending/` → `push`.
- Don't bake the operator's identity or current active DB into
  commits.
- Don't try to encrypt the env files. Filesystem perms (0600 on
  env files, 0700 on `keys/`) are sufficient.
- Don't introduce SQLite or any embedded DB. JSON files on disk.
- Don't merge files in pending/ — each change is atomic.

## License

MIT.
