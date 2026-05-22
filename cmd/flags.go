package cmd

import (
	"strconv"
	"strings"
)

// HasFlag reports whether any of the given names appears as a
// stand-alone arg.
func HasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// GetOption returns the value of the FIRST occurrence of any of the
// given flag names. Accepts both forms:
//
//	--flag value     → returns "value"
//	--flag=value     → returns "value"
//
// Returns "" when no occurrence is found or the flag has no value.
func GetOption(args []string, names ...string) string {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(a, n+"=") {
				return strings.SplitN(a, "=", 2)[1]
			}
		}
	}
	return ""
}

// GetNumber returns the first numeric value found for any of the
// given flag names, or defaultVal when none parses cleanly.
func GetNumber(args []string, names []string, defaultVal int) int {
	v := GetOption(args, names...)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return defaultVal
	}
	return n
}

// RemoveFlag returns args with every standalone occurrence of `flag`
// removed. Doesn't consume a following value (use RemoveOption when
// the flag carries one).
func RemoveFlag(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == flag {
			continue
		}
		out = append(out, a)
	}
	return out
}

// RemoveOption removes BOTH `flag` and its following value from
// args. Used by top-level dispatch to strip `--db <name>` before
// sub-commands re-parse the rest.
func RemoveOption(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		if a == flag {
			skip = true
			continue
		}
		if strings.HasPrefix(a, flag+"=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// FirstPositional returns the first arg that doesn't start with `-`
// and isn't the value-side of a known value-flag. Used by commands
// that take a positional arg (journal id, dbname, …) alongside
// global flags like `--db`.
func FirstPositional(args []string, valueFlags ...string) string {
	skip := false
	for _, a := range args {
		if skip {
			skip = false
			continue
		}
		for _, vf := range valueFlags {
			if a == vf {
				skip = true
				goto next
			}
		}
		if len(a) > 0 && a[0] == '-' {
			continue
		}
		return a
	next:
	}
	return ""
}
