// Package bytesize reads byte counts written with SI or binary unit suffixes.
package bytesize

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// overflow is the first value a byte count may not reach.
//
// It is two to the sixty-third, one past the largest int64, because that is the
// nearest bound a float64 can express exactly: math.MaxInt64 itself has no
// exact float64 form, so comparing against it would let a value through that
// the conversion then wraps to a negative number. A negative limit reads as no
// limit at all further down, which turned a mistyped size into a guard that was
// silently switched off.
const overflow = float64(1 << 63)

// units maps a suffix to its multiplier, the longest suffix of each family
// first so that MiB is never read as M. Both SI and binary suffixes are
// accepted because build scripts and configuration files write both.
var units = []struct {
	suffix     string
	multiplier int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	{"B", 1},
}

// Format writes a byte count with a binary unit suffix, using one decimal place
// for anything above a kibibyte. It is the inverse of Parse only in spirit:
// it rounds, so it belongs in summaries and never in a lock file.
func Format(count int64) string {
	if count < 1<<10 {
		return fmt.Sprintf("%d B", count)
	}
	value := float64(count)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= 1 << 10
		if value < 1<<10 {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/(1<<10))
}

// Parse reads a byte count such as 512, 8KiB, or 1.5MB.
func Parse(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("byte count %q is empty", value)
	}
	upper := strings.ToUpper(trimmed)
	for _, unit := range units {
		if !strings.HasSuffix(upper, unit.suffix) {
			continue
		}
		amount, err := strconv.ParseFloat(strings.TrimSpace(trimmed[:len(trimmed)-len(unit.suffix)]), 64)
		if err != nil {
			return 0, fmt.Errorf("byte count %q is invalid", value)
		}
		// ParseFloat accepts "NaN" and "Inf", so "NaNB" and "InfB" arrive here as
		// numbers. Neither is negative and neither converts to anything an int64
		// can hold, so both would otherwise pass every check below.
		if math.IsNaN(amount) || math.IsInf(amount, 0) {
			return 0, fmt.Errorf("byte count %q is invalid", value)
		}
		if amount < 0 {
			return 0, fmt.Errorf("byte count %q must not be negative", value)
		}
		scaled := amount * float64(unit.multiplier)
		if scaled >= overflow {
			return 0, fmt.Errorf("byte count %q is larger than %d bytes", value, int64(math.MaxInt64))
		}
		return int64(scaled), nil
	}
	amount, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("byte count %q is invalid", value)
	}
	if amount < 0 {
		return 0, fmt.Errorf("byte count %q must not be negative", value)
	}
	return amount, nil
}
