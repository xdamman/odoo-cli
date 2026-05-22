package cmd

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Database holds the resolved credentials for one Odoo instance.
// The URL is normalised (trailing slash stripped) and DB is derived
// from the URL hostname when ODOO_DATABASE is unset in the env file.
type Database struct {
	Name     string // local slug (filename minus .env)
	URL      string // https://myorg.odoo.com
	DB       string // database name on Odoo (often == hostname's first label)
	Login    string
	Password string
	EnvPath  string // absolute path to the env file we loaded from
}

// LoadDatabase reads ~/.odoo/databases/<name>.env and returns the
// resolved Database. Returns os.ErrNotExist when no such file
// exists — callers print a hint to run `odoo setup`.
func LoadDatabase(name string) (*Database, error) {
	if name == "" {
		return nil, fmt.Errorf("database name is empty")
	}
	path := DatabaseEnvPath(name)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	values := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		values[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	db := &Database{
		Name:     name,
		URL:      strings.TrimRight(values["ODOO_URL"], "/"),
		DB:       values["ODOO_DATABASE"],
		Login:    values["ODOO_LOGIN"],
		Password: values["ODOO_PASSWORD"],
		EnvPath:  path,
	}
	if db.DB == "" {
		db.DB = dbFromURL(db.URL)
	}
	if db.URL == "" || db.Login == "" || db.Password == "" {
		return db, fmt.Errorf("database %q is missing required fields (ODOO_URL / ODOO_LOGIN / ODOO_PASSWORD) in %s", name, path)
	}
	return db, nil
}

// SaveDatabase writes a new .env file (or overwrites an existing
// one) for the given database. Always 0600.
func SaveDatabase(db *Database) error {
	if db == nil || db.Name == "" {
		return fmt.Errorf("nil or unnamed database")
	}
	if err := EnsureAppRoot(); err != nil {
		return err
	}
	path := DatabaseEnvPath(db.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "# odoo-cli database config — written by `odoo setup`\n")
	fmt.Fprintf(&b, "ODOO_URL=%s\n", db.URL)
	if db.DB != "" {
		fmt.Fprintf(&b, "ODOO_DATABASE=%s\n", db.DB)
	}
	fmt.Fprintf(&b, "ODOO_LOGIN=%s\n", db.Login)
	fmt.Fprintf(&b, "ODOO_PASSWORD=%s\n", db.Password)
	return os.WriteFile(path, []byte(b.String()), 0600)
}

// ListDatabases returns the names of every configured database
// (every *.env file under ~/.odoo/databases/). Empty when the dir
// doesn't exist or holds no env files.
func ListDatabases() []string {
	entries, err := os.ReadDir(DatabasesDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".env"))
	}
	sort.Strings(out)
	return out
}

// ResolveActive returns the database to use for this invocation.
// Resolution order:
//
//  1. `--db <name>` flag in args (per-invocation override).
//  2. state.json's `activeDb` field (persistent default).
//
// Returns an error with a friendly hint when nothing resolves.
func ResolveActive(args []string) (*Database, error) {
	if name := GetOption(args, "--db"); name != "" {
		return LoadDatabase(name)
	}
	state, _ := LoadState()
	if state.ActiveDB != "" {
		return LoadDatabase(state.ActiveDB)
	}
	names := ListDatabases()
	if len(names) == 0 {
		return nil, fmt.Errorf("no database configured. Run `odoo setup` to add one")
	}
	return nil, fmt.Errorf("no active database set. Run `odoo switch <name>` (configured: %s)", strings.Join(names, ", "))
}

// dbFromURL extracts the first hostname label as the default DB
// name when ODOO_DATABASE is unset. `https://acme.odoo.com` → `acme`.
func dbFromURL(url string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	u = strings.TrimSuffix(u, "/")
	host, _, _ := strings.Cut(u, "/")
	first, _, _ := strings.Cut(host, ".")
	return first
}

// DatabaseHost returns the bare host of the configured URL (used
// for printing references and building URIs).
func (d *Database) Host() string {
	u := strings.TrimPrefix(strings.TrimPrefix(d.URL, "https://"), "http://")
	host, _, _ := strings.Cut(u, "/")
	return host
}

// SanitizeDBName normalises a user-typed name into a safe file slug.
// Lowercases, replaces whitespace with `-`, drops anything else.
func SanitizeDBName(raw string) string {
	raw = strings.TrimSpace(strings.ToLower(raw))
	var b strings.Builder
	for _, r := range raw {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// EnvFilePath joins a database name onto the databases dir without
// loading the file. Useful for "do you already have this?" checks.
func EnvFilePath(name string) string {
	return filepath.Join(DatabasesDir(), name+".env")
}
