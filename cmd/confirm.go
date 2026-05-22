package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// confirmTTY prompts the operator for y/N confirmation via the
// controlling terminal at /dev/tty. Used by the piped commands
// (attach / assign / unreconcile) — stdin is consumed by JSONL, so
// we can't read the confirmation through it, but the operator's
// terminal is still reachable via /dev/tty.
//
// Returns:
//   (true, nil)  → operator typed y/yes
//   (false, nil) → operator typed anything else / hit Enter
//   (false, err) → /dev/tty isn't openable (caller falls back to
//                  "refusing without --yes" semantics)
//
// The prompt always mentions --yes so an operator can shortcut the
// prompt next time.
func confirmTTY(prompt string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer tty.Close()
	fmt.Fprintf(tty, "%s%s%s [y/N] (or re-run with --yes to skip this prompt) ",
		Fmt.Bold, prompt, Fmt.Reset)
	r := bufio.NewReader(tty)
	line, rerr := r.ReadString('\n')
	if rerr != nil && line == "" {
		return false, nil
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}
