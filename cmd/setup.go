package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// Setup walks the operator through adding a new Odoo database. Reads
// stdin interactively; refuses to run without a TTY. Validates the
// credentials against Odoo before writing the .env file.
//
// Usage:
//
//	odoo setup                      # walk through prompts
//	odoo setup --name <n> --url <u> # non-interactive (all flags required)
//	                  --login <l> --password <p> [--db <odoo-db>]
func Setup(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printSetupHelp()
		return nil
	}

	// Non-interactive path: all four core flags must be present.
	flagName := GetOption(args, "--name")
	flagURL := GetOption(args, "--url")
	flagLogin := GetOption(args, "--login")
	flagPass := GetOption(args, "--password")
	flagOdooDB := GetOption(args, "--odoo-db")

	var db Database
	if flagName != "" || flagURL != "" || flagLogin != "" || flagPass != "" {
		if flagName == "" || flagURL == "" || flagLogin == "" || flagPass == "" {
			return fmt.Errorf("non-interactive setup needs all of --name, --url, --login, --password (got name=%q url=%q login=%q pass-set=%v)",
				flagName, flagURL, flagLogin, flagPass != "")
		}
		db = Database{
			Name:     SanitizeDBName(flagName),
			URL:      strings.TrimRight(flagURL, "/"),
			DB:       flagOdooDB,
			Login:    flagLogin,
			Password: flagPass,
		}
	} else {
		if !isTTY() {
			return fmt.Errorf("`odoo setup` needs a TTY for the interactive walkthrough — pass --name/--url/--login/--password explicitly to skip prompts")
		}
		var err error
		db, err = setupInteractive()
		if err != nil {
			return err
		}
	}

	if db.DB == "" {
		db.DB = dbFromURL(db.URL)
	}

	// Don't accidentally overwrite an existing config without
	// explicit consent.
	if _, err := os.Stat(DatabaseEnvPath(db.Name)); err == nil && !HasFlag(args, "--force") {
		return fmt.Errorf("database %q already configured at %s — pass --force to overwrite", db.Name, DatabaseEnvPath(db.Name))
	}

	// Validate before writing so we don't leave a broken config on disk.
	fmt.Printf("\n%s● Verifying credentials against %s%s\n", Fmt.Dim, db.URL, Fmt.Reset)
	uid, err := Auth(db.URL, db.DB, db.Login, db.Password)
	if err != nil {
		return fmt.Errorf("auth failed: %v", err)
	}
	fmt.Printf("%s✓ Authenticated as uid=%d on db=%s%s\n", Fmt.Green, uid, db.DB, Fmt.Reset)

	if err := SaveDatabase(&db); err != nil {
		return fmt.Errorf("save .env: %v", err)
	}
	if err := EnsureCacheDirs(db.Name); err != nil {
		return fmt.Errorf("create cache dirs: %v", err)
	}
	if err := SetActiveDB(db.Name); err != nil {
		return fmt.Errorf("set active db: %v", err)
	}
	fmt.Printf("\n%s✓ Saved %s (active)%s\n", Fmt.Green, db.Name, Fmt.Reset)
	fmt.Printf("  %sEnv:%s   %s\n", Fmt.Dim, Fmt.Reset, DatabaseEnvPath(db.Name))
	fmt.Printf("  %sCache:%s %s\n", Fmt.Dim, Fmt.Reset, CacheDir(db.Name))
	fmt.Printf("\n  Next: %sodoo pull%s to populate the cache, then %sodoo journals --all%s to browse.\n\n",
		Fmt.Cyan, Fmt.Reset, Fmt.Cyan, Fmt.Reset)
	return nil
}

func setupInteractive() (Database, error) {
	r := bufio.NewReader(os.Stdin)
	var db Database

	fmt.Printf("\n%sodoo setup%s — add a new Odoo database\n", Fmt.Bold, Fmt.Reset)
	fmt.Printf("%sAll prompts can be re-run; nothing is written until validation passes.%s\n\n", Fmt.Dim, Fmt.Reset)

	for {
		raw, err := prompt(r, "Name (local slug — e.g. acme, prod, test): ")
		if err != nil {
			return db, err
		}
		db.Name = SanitizeDBName(raw)
		if db.Name == "" {
			fmt.Printf("  %sname can't be empty%s\n", Fmt.Yellow, Fmt.Reset)
			continue
		}
		if _, err := os.Stat(DatabaseEnvPath(db.Name)); err == nil {
			fmt.Printf("  %sa database named %q already exists. Use a different name or pass --force later.%s\n", Fmt.Yellow, db.Name, Fmt.Reset)
			continue
		}
		break
	}

	for {
		raw, err := prompt(r, "URL (e.g. https://acme.odoo.com): ")
		if err != nil {
			return db, err
		}
		raw = strings.TrimRight(raw, "/")
		if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
			fmt.Printf("  %sURL must start with http:// or https://%s\n", Fmt.Yellow, Fmt.Reset)
			continue
		}
		db.URL = raw
		break
	}

	defaultOdooDB := dbFromURL(db.URL)
	odooDB, err := prompt(r, fmt.Sprintf("Odoo database name [%s]: ", defaultOdooDB))
	if err != nil {
		return db, err
	}
	if odooDB == "" {
		db.DB = defaultOdooDB
	} else {
		db.DB = odooDB
	}

	login, err := prompt(r, "Login (email): ")
	if err != nil {
		return db, err
	}
	db.Login = login

	pass, err := promptPassword(r, "Password: ")
	if err != nil {
		return db, err
	}
	db.Password = pass

	return db, nil
}

func prompt(r *bufio.Reader, label string) (string, error) {
	fmt.Print("  " + label)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func promptPassword(r *bufio.Reader, label string) (string, error) {
	fmt.Print("  " + label)
	// Hide echo when stdin is a real terminal.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func printSetupHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo setup%s — add a new Odoo database

%sUSAGE%s
  %sodoo setup%s                                 # interactive walkthrough (TTY required)
  %sodoo setup%s --name N --url U --login L --password P [--odoo-db D]

%sFLAGS%s
  %s--name%s    Local slug (filename minus .env)
  %s--url%s     Odoo URL — e.g. https://acme.odoo.com
  %s--odoo-db%s The DB name on the Odoo server (defaults to the URL's first label)
  %s--login%s   Email / username
  %s--password%s Plain-text password (avoid in shell history; prefer interactive)
  %s--force%s   Overwrite an existing config with the same --name

%sBEHAVIOUR%s
  1. Reads the four fields above (interactive or via flags).
  2. Calls common.authenticate against Odoo to verify them.
  3. On success: writes ~/.odoo/databases/<name>.env (0600), creates
     ~/.odoo/cache/<name>/, and sets <name> as the active database.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
	)
}
