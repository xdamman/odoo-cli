package cmd

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// updateRepo is the GitHub `owner/name` from which releases are
// fetched. Hard-coded because there's only ever one source-of-truth
// repo for this CLI's binaries.
const updateRepo = "xdamman/odoo-cli"

// Update is `odoo update` — self-update against GitHub releases.
//
// Flow:
//  1. Query api.github.com for the latest release tag (or use the
//     tag passed via --version).
//  2. Compare with the running binary's Version (set at build time
//     via -X main.VERSION).
//  3. Download the matching odoo-<os>-<arch>.tar.gz + checksums.txt,
//     verify the SHA256, extract the `odoo` binary.
//  4. Atomically replace the running executable (rename(2) — the
//     kernel keeps the old inode mapped for the current process).
//
// Honours --check (just print versions), --yes (skip TTY prompt),
// and --version <tag> (pin to a specific release).
func Update(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printUpdateHelp()
		return nil
	}
	checkOnly := HasFlag(args, "--check")
	assumeYes := HasFlag(args, "--yes", "-y")
	pinned := strings.TrimSpace(GetOption(args, "--version"))

	cur := strings.TrimSpace(Version)
	if cur == "" {
		cur = "dev"
	}

	fmt.Printf("\n%s● Checking %s for updates …%s\n", Fmt.Dim, updateRepo, Fmt.Reset)
	latest, err := fetchLatestTag()
	if err != nil {
		return fmt.Errorf("fetch latest release: %v", err)
	}
	target := latest
	if pinned != "" {
		target = normalizeTag(pinned)
	}

	fmt.Printf("  current: %s%s%s\n", Fmt.Bold, cur, Fmt.Reset)
	fmt.Printf("  latest:  %s%s%s\n", Fmt.Bold, latest, Fmt.Reset)
	if target != latest {
		fmt.Printf("  target:  %s%s%s\n", Fmt.Bold, target, Fmt.Reset)
	}

	if cur == target {
		fmt.Printf("\n%s✓ Already on %s — nothing to do.%s\n\n", Fmt.Green, target, Fmt.Reset)
		return nil
	}
	if checkOnly {
		fmt.Printf("\n%sRun %sodoo update%s%s to install %s.%s\n\n",
			Fmt.Dim, Fmt.Cyan, Fmt.Reset, Fmt.Dim, target, Fmt.Reset)
		return nil
	}

	// The release matrix is linux + darwin × amd64 + arm64. Anything
	// else can't be self-updated from a tarball — bail with a
	// helpful message instead of downloading something useless.
	osName := runtime.GOOS
	arch := runtime.GOARCH
	if osName != "linux" && osName != "darwin" {
		return fmt.Errorf("self-update only supports linux/darwin (you're on %s/%s — build from source)", osName, arch)
	}
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("self-update only supports amd64/arm64 (you're on %s/%s — build from source)", osName, arch)
	}

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(selfPath); err == nil {
		selfPath = resolved
	}

	if !assumeYes {
		if !isTTY() {
			return fmt.Errorf("refusing to overwrite %s on a non-TTY without --yes", selfPath)
		}
		fmt.Printf("\n%sReplace %s with %s?%s [Y/n] ",
			Fmt.Bold, selfPath, target, Fmt.Reset)
		resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		resp = strings.ToLower(strings.TrimSpace(resp))
		if resp == "n" || resp == "no" {
			fmt.Println("  cancelled.")
			return nil
		}
	}

	asset := fmt.Sprintf("odoo-%s-%s.tar.gz", osName, arch)
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", updateRepo, target)

	fmt.Printf("\n%s● Downloading %s …%s\n", Fmt.Dim, asset, Fmt.Reset)
	tarball, err := downloadBytes(base + "/" + asset)
	if err != nil {
		return fmt.Errorf("download tarball: %v", err)
	}

	fmt.Printf("%s● Verifying checksum …%s\n", Fmt.Dim, Fmt.Reset)
	sums, err := downloadBytes(base + "/checksums.txt")
	if err != nil {
		return fmt.Errorf("download checksums.txt: %v", err)
	}
	expected, ok := pickChecksum(sums, asset)
	if !ok {
		return fmt.Errorf("checksum for %s not listed in checksums.txt", asset)
	}
	got := sha256.Sum256(tarball)
	if hex.EncodeToString(got[:]) != expected {
		return fmt.Errorf("checksum mismatch — expected %s, got %s", expected, hex.EncodeToString(got[:]))
	}

	fmt.Printf("%s● Extracting odoo binary …%s\n", Fmt.Dim, Fmt.Reset)
	bin, err := extractTarballBinary(tarball, "odoo")
	if err != nil {
		return fmt.Errorf("extract binary: %v", err)
	}

	fmt.Printf("%s● Installing to %s …%s\n", Fmt.Dim, selfPath, Fmt.Reset)
	if err := replaceBinary(selfPath, bin); err != nil {
		return err
	}

	fmt.Printf("\n%s✓ Updated %s → %s%s\n\n", Fmt.Green, cur, target, Fmt.Reset)
	return nil
}

// fetchLatestTag hits the public releases endpoint. No auth needed
// for the unauthenticated rate-limit window (60/hr/IP) which is more
// than enough for self-update.
func fetchLatestTag() (string, error) {
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+updateRepo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "odoo-cli-update/"+Version)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("github api %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.TagName == "" {
		return "", fmt.Errorf("github api returned no tag_name")
	}
	return payload.TagName, nil
}

// downloadBytes fetches the URL with a 2-minute ceiling (tarballs
// are ~6-8 MB so anything slower is a network problem, not a slow
// CDN). Returns the raw body on a 200.
func downloadBytes(url string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "odoo-cli-update/"+Version)
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// pickChecksum walks the "sha256  filename" lines emitted by
// `sha256sum *.tar.gz > checksums.txt` and returns the expected sum
// for the asset, or "", false when it isn't listed.
func pickChecksum(file []byte, asset string) (string, bool) {
	for _, line := range strings.Split(string(file), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		if fields[1] == asset || fields[1] == "*"+asset {
			return fields[0], true
		}
	}
	return "", false
}

// extractTarballBinary walks the tar.gz looking for a regular file
// named `binName` (anywhere in the tree — the release packages put
// it under odoo-<os>-<arch>/odoo). Returns the file bytes.
func extractTarballBinary(tarball []byte, binName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s not found in tarball", binName)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) != binName {
			continue
		}
		return io.ReadAll(tr)
	}
}

// replaceBinary writes `bin` to a sibling temp file (so it lands on
// the same filesystem as the target) then rename(2)s it over the
// running executable. rename across the same filesystem is atomic;
// the running process keeps executing from the old inode the kernel
// still has mapped.
func replaceBinary(target string, bin []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".odoo-update-*")
	if err != nil {
		// Most common cause: target is in /usr/local/bin and current
		// user can't write the parent dir.
		return fmt.Errorf("cannot write to %s: %v (try `sudo odoo update`)", dir, err)
	}
	tmpPath := tmp.Name()
	rollback := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		rollback()
		return fmt.Errorf("write temp binary: %v", err)
	}
	if err := tmp.Chmod(0755); err != nil {
		tmp.Close()
		rollback()
		return fmt.Errorf("chmod temp binary: %v", err)
	}
	if err := tmp.Close(); err != nil {
		rollback()
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		rollback()
		return fmt.Errorf("replace %s: %v (try `sudo odoo update`)", target, err)
	}
	return nil
}

// normalizeTag accepts "0.0.1", "v0.0.1", or "V0.0.1" and returns
// the canonical "v0.0.1" form GitHub release tags use.
func normalizeTag(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if s[0] == 'V' {
		s = "v" + s[1:]
	}
	if !strings.HasPrefix(s, "v") {
		s = "v" + s
	}
	return s
}

func printUpdateHelp() {
	f := Fmt
	fmt.Printf(`
%sodoo update%s — self-update from GitHub releases

%sUSAGE%s
  %sodoo update%s                     Check + install if newer, with TTY confirm
  %sodoo update --check%s             Just check; print current vs. latest
  %sodoo update --yes%s               Install without confirmation
  %sodoo update --version v0.0.2%s    Pin to a specific release tag

%sBEHAVIOUR%s
  1. Queries https://api.github.com/repos/%s/releases/latest for the
     tag name.
  2. Compares with the running binary's embedded version.
  3. Downloads odoo-<os>-<arch>.tar.gz + checksums.txt from the
     release, verifies SHA256, extracts the %sodoo%s binary.
  4. Atomically replaces the running executable (rename(2) — safe
     while the current process is still running).

%sLIMITATIONS%s
  • Linux/macOS × amd64/arm64 only (the release matrix). Build from
    source on anything else.
  • If %sodoo%s sits in a system path (/usr/local/bin, /opt, …), the
    rename will fail with EACCES — re-run with %ssudo odoo update%s.
  • Dev builds (version "dev") will always be flagged as outdated;
    use %s--check%s if you want to inspect without overwriting.

`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		updateRepo,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
	)
}
