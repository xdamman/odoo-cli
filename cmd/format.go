package cmd

import (
	"fmt"
	"math"
	"strings"
)

// Truncate trims s to length runes, appending "…" when truncation
// happens. Rune-based so unicode width stays correct.
func Truncate(s string, length int) string {
	runes := []rune(s)
	if len(runes) <= length {
		return s
	}
	if length <= 1 {
		return string(runes[:length])
	}
	return string(runes[:length-1]) + "…"
}

// PadRight returns s right-padded with spaces up to width. Width is
// measured in runes (DisplayWidth), not bytes.
func PadRight(s string, width int) string {
	if n := width - DisplayWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// PadLeft is the right-aligned counterpart of PadRight.
func PadLeft(s string, width int) string {
	if n := width - DisplayWidth(s); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

// DisplayWidth = rune count. Sufficient for monospaced terminal
// rendering where one rune = one cell. (Doesn't handle CJK wide
// chars or zero-width joiners — odoo data is mostly Latin.)
func DisplayWidth(s string) int {
	return len([]rune(s))
}

// FmtEUR formats a float as "12,345.67 EUR" (no currency symbol, trailing code).
func FmtEUR(v float64) string {
	return fmtNumber(math.Abs(v)) + " EUR"
}

// FmtEURSigned adds a +/- prefix.
func FmtEURSigned(v float64) string {
	if v >= 0 {
		return "+" + FmtEUR(v)
	}
	return "-" + FmtEUR(-v)
}

// FmtAmount formats a float with the given currency suffix. EUR
// reuses FmtEUR; everything else gets a two-decimal number + the
// uppercased currency code.
func FmtAmount(v float64, currency string) string {
	currency = strings.TrimSpace(currency)
	if currency == "" || strings.EqualFold(currency, "EUR") {
		return FmtEUR(v)
	}
	return fmt.Sprintf("%s %s", fmtNumber(math.Abs(v)), strings.ToUpper(currency))
}

func fmtNumber(v float64) string {
	intPart := int64(v)
	decPart := v - float64(intPart)
	dec := fmt.Sprintf("%.2f", decPart)[1:] // ".67"
	str := fmt.Sprintf("%d", intPart)
	// Thousands separator (comma).
	var b strings.Builder
	n := len(str)
	for i, c := range str {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String() + dec
}

// FirstNonEmpty returns the first non-blank string from xs.
func FirstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// CollapseWhitespace folds tabs/newlines/runs-of-spaces into single
// spaces. Used so multi-line Odoo descriptions render as one row.
func CollapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
