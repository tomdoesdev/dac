// Package bytesize parses byte counts and writes compact byte-count displays.
package bytesize

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// unit gives one accepted suffix its number of bytes.
type unit struct {
	suffix     string
	multiplier int64
}

// units are longest first, so a binary suffix is not mistaken for its byte suffix.
var units = []unit{
	{"EIB", 1 << 60}, {"PIB", 1 << 50}, {"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
	{"EB", 1000 * 1000 * 1000 * 1000 * 1000 * 1000}, {"PB", 1000 * 1000 * 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"MB", 1000 * 1000}, {"KB", 1000},
	// K through T are kept as the binary aliases DAC has always accepted.
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	{"B", 1},
}

// displayUnits are the IEC units Humanize writes, in increasing order.
var displayUnits = []unit{
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"PiB", 1 << 50}, {"EiB", 1 << 60},
}

// Humanize writes a byte count with a binary unit suffix. Counts below a
// kibibyte, including negative values, stay in bytes. Larger counts use one
// decimal place and are rounded for display, so this text is not a lossless
// serialization and should not be used to persist a byte count.
func Humanize(count int64) string {
	if count < 1<<10 {
		return strconv.FormatInt(count, 10) + " B"
	}
	for index, current := range displayUnits {
		value := float64(count) / float64(current.multiplier)
		if value >= 1<<10 && index < len(displayUnits)-1 {
			continue
		}
		text := strconv.FormatFloat(value, 'f', 1, 64)
		// A rounded 1024.0 belongs in the next column. Otherwise a count just
		// below a unit boundary reads larger than the unit it is displayed in.
		if text == "1024.0" && index < len(displayUnits)-1 {
			continue
		}
		return text + " " + current.suffix
	}
	return ""
}

// Parse reads a non-negative byte count such as 512, 8KiB, or 1.5MB. It
// accepts a leading plus sign and whitespace before the optional suffix.
// Units are case-insensitive: B and no suffix mean bytes; KB through EB are
// decimal units; KiB through EiB are binary units; and K through T remain
// binary aliases for compatibility. A decimal amount must resolve to a whole
// int64 byte count.
func Parse(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("byte count %q is empty", value)
	}

	amountText, multiplier := splitUnit(trimmed)
	amountText = strings.TrimSpace(amountText)
	if strings.HasPrefix(amountText, "-") {
		return 0, fmt.Errorf("byte count %q must not be negative", value)
	}
	if !isDecimal(amountText) {
		return 0, fmt.Errorf("byte count %q is invalid", value)
	}

	// Rat keeps both decimal text and scaling exact. Float64 used to round
	// valid suffixed counts before the range check, changing their byte value.
	amount, ok := new(big.Rat).SetString(amountText)
	if !ok {
		return 0, fmt.Errorf("byte count %q is invalid", value)
	}
	scaled := amount.Mul(amount, big.NewRat(multiplier, 1))
	if !scaled.IsInt() {
		return 0, fmt.Errorf("byte count %q does not resolve to whole bytes", value)
	}
	bytes := scaled.Num()
	if !bytes.IsInt64() {
		return 0, fmt.Errorf("byte count %q is larger than %d bytes", value, int64(math.MaxInt64))
	}
	return bytes.Int64(), nil
}

// splitUnit separates a supported suffix from value. A missing suffix means bytes.
func splitUnit(value string) (string, int64) {
	upper := strings.ToUpper(value)
	for _, current := range units {
		if strings.HasSuffix(upper, current.suffix) {
			return value[:len(value)-len(current.suffix)], current.multiplier
		}
	}
	return value, 1
}

// isDecimal reports whether text is the restricted base-ten amount grammar
// Parse supports. Keeping the grammar narrow avoids silently treating Go float
// literals such as exponents or hexadecimal values as configuration sizes.
func isDecimal(text string) bool {
	if strings.HasPrefix(text, "+") {
		text = text[1:]
	}
	if text == "" {
		return false
	}
	dot := false
	digitsBeforeDot := 0
	digitsAfterDot := 0
	for _, character := range text {
		switch {
		case character >= '0' && character <= '9':
			if dot {
				digitsAfterDot++
			} else {
				digitsBeforeDot++
			}
		case character == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digitsBeforeDot > 0 && (!dot || digitsAfterDot > 0)
}
