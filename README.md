# odoo-cli

A local-first command-line tool for everyday Odoo bookkeeping operations.
List journals, drill into invoices and bills, reconcile bank lines, push
queued changes back to Odoo — without a browser tab.

## Install

```
go install github.com/xdamman/odoo@latest
```

The binary is named `odoo`. Add `$(go env GOPATH)/bin` to your `$PATH`
if you haven't already.

## Quickstart

```
$ odoo setup
  Name (local slug — e.g. acme, prod, test): acme
  URL (e.g. https://acme.odoo.com): https://acme.odoo.com
  Odoo database name [acme]:
  Login (email): finance@acme.example
  Password: ********

  ● Verifying credentials against https://acme.odoo.com
  ✓ Authenticated as uid=42 on db=acme

✓ Saved acme (active)
  Env:   ~/.odoo/databases/acme.env
  Cache: ~/.odoo/cache/acme

  Next: odoo pull to populate the cache, then odoo journals --all to browse.
```

```
$ odoo journals
db: acme

1 journal — favorites only (use --all or --search to widen)

  ★ ID    Type  Name                              Code  Currency
    34    bank  KBC Brussels Operating Account    BANK  EUR

(cache: 2m ago — run `odoo pull` to refresh)
```

```
$ odoo pull
db: acme

● Authenticating against https://acme.odoo.com …
● Pulling journals list …
  ✓ 18 journals
● Pulling lines for journal #34 KBC Brussels Operating Account …
  ✓ 412 lines
● Pulling open invoices …
  ✓ 72 invoices
● Pulling open bills …
  ✓ 8 bills
● Pulling partner index …
  ✓ 894 partners

✓ Pulled in 3.4s
  Next: odoo journals · odoo journals <id> reconcile -i · odoo push --yes
```

## Commands

| Command | What it does |
|---|---|
| `odoo setup` | Add a new Odoo database (interactive walkthrough; validates credentials before writing). |
| `odoo switch [name]` | List configured databases, mark the active one with ★, switch. |
| `odoo journals [--search KW] [--all]` | List journals — favorites only by default. |
| `odoo journals <id>` | Show details for one journal (or one journal code: `odoo journals BANK`). |
| `odoo journals <id> favorite` / `unfavorite` | Toggle the journal's favorite status in `~/.odoo/cache/<db>/favorites.json`. |
| `odoo journals <id> reconcile [-i] [--yes]` | Reconcile unmatched bank statement lines with open invoices/bills. (TUI w/ `-i`.) |
| `odoo pull` | Refresh `~/.odoo/cache/<db>/` from Odoo: journals list, favorite-journal lines, open invoices/bills, partner index. Read-only. |
| `odoo push [--yes]` | Replay queued pending changes against Odoo. Successful changes archive to `sent/`. |
| `odoo sync [--yes]` | `odoo pull && odoo push`. |

Every command supports `--help`, which short-circuits at the top of
the dispatch and returns immediately.

## Multi-database

Each database lives in its own `.env` file under `~/.odoo/databases/`.
The active database is tracked in `~/.odoo/state.json` and shown at the
top of every command's output:

```
db: acme
```

Override for a single invocation:

```
$ odoo --db test journals --search bank
db: test
…
```

`odoo switch` shows the configured databases (sorted by last-used) and
lets you pick by name or number. The ★ marks the current active one.

## On disk

```
~/.odoo/
  databases/<dbname>.env   ODOO_URL=…, ODOO_LOGIN=…, ODOO_PASSWORD=…, ODOO_DATABASE=…
                            (0600; one file per database)
  state.json                Active database + last-used timestamps
  cache/<dbname>/
    journals/list.json      Every journal (id, name, code, type, currency)
    journals/<id>.json      Per-favorite-journal bank statement lines
    invoices.json           Open + partially-paid out_invoice / out_refund
    bills.json              Open + partially-paid in_invoice / in_refund
    partners.json           id → name + IBANs
    favorites.json          {"journals":[id,…]}
    pending/                One JSON per queued local change
    sent/                   Archive of successfully-pushed changes
    _last_sync.json         pulledAt + pushedAt + per-bucket counts
  keys/                     Reserved for signing material (SSH-style)
```

Override the root with `ODOO_CLI_ROOT=/some/path` (useful for tests).

## Pending changes

Local mutations (reconcile actions, future categorizations, etc.) are
queued as JSON files under `~/.odoo/cache/<dbname>/pending/`. Each has
a `kind`, a `payload`, and a `createdAt`. `odoo push` walks the queue,
dispatches each change to its apply function, and either archives the
file to `sent/` or stamps `lastError` + `attempts` and leaves it in
place for the next run.

The flow is intentionally explicit: queue → preview (dry-run) →
apply (`--yes`). Nothing writes to Odoo from a single command except
`push`, and `push` itself requires `--yes` or a TTY prompt.

## Authentication

Credentials live in `~/.odoo/databases/<dbname>.env` (0600). SSH-style
private material (currently unused; reserved for future signing keys)
lives in `~/.odoo/keys/`, kept entirely outside the cache so a future
"share my cache" feature can't accidentally include it.

The CLI never manages credentials beyond reading the `.env` file. Use
your shell's password manager (`pass`, `1password-cli`, …) to inject
the env file's contents on demand if you don't want plain-text on disk.

## Philosophy

This tool is a sibling project of [chb](https://github.com/CommonsHub/chb).
The patterns are shared (local-first JSON cache, pull/generate/push
pipeline, dim-by-default with `-v`, `--help` short-circuit, multi-DB
state), the scope is narrower: Odoo-only, no providers, no generated
files beyond the cache.

See [CLAUDE.md](./CLAUDE.md) for the rules of the road for
contributors (and for Claude when iterating on this codebase).

## License

MIT — see [LICENSE](./LICENSE).
