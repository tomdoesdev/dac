package bytesize

import (
	"fmt"
	"math"
	"testing"
)

func TestParseReadsSupportedUnitsExactly(t *testing.T) {
	cases := []struct {
		value string
		want  int64
	}{
		{"0", 0},
		{"512", 512},
		{"1B", 1},
		{" +1 B ", 1},
		{"8KiB", 8 << 10},
		{"1MiB", 1 << 20},
		{"2GiB", 2 << 30},
		{"1TiB", 1 << 40},
		{"1PiB", 1 << 50},
		{"1EiB", 1 << 60},
		{"1KB", 1000},
		{"1MB", 1000 * 1000},
		{"1GB", 1000 * 1000 * 1000},
		{"1TB", 1000 * 1000 * 1000 * 1000},
		{"1PB", 1000 * 1000 * 1000 * 1000 * 1000},
		{"9EB", 9 * 1000 * 1000 * 1000 * 1000 * 1000 * 1000},
		{"1K", 1 << 10},
		{"1M", 1 << 20},
		{"1G", 1 << 30},
		{"1T", 1 << 40},
		{" 1.5kib ", 1536},
		{"1.1KB", 1100},
		{"0.5KiB", 512},
		{"9223372036854775807B", math.MaxInt64},
		{"9007199254740993B", 9007199254740993},
	}
	for _, test := range cases {
		got, err := Parse(test.value)
		if err != nil {
			t.Errorf("Parse(%q) failed: %v", test.value, err)
			continue
		}
		if got != test.want {
			t.Errorf("Parse(%q) = %d, want %d", test.value, got, test.want)
		}
	}
}

func TestParseRejectsInvalidOrInexactCounts(t *testing.T) {
	for _, value := range []string{
		"", "  ", "-1", "-1MiB", "MiB", "1XiB", "one", "1MiB/s",
		"1e3B", "0x1p10B", "1_000B", ".5KiB", "1.KiB", "+", "1.1B",
		"NaNB", "InfB", "8EiB", "10EB", "99999999TiB",
	} {
		if got, err := Parse(value); err == nil {
			t.Errorf("Parse(%q) = %d, want an error", value, got)
		}
	}
}

func TestHumanizeUsesBinaryDisplayUnits(t *testing.T) {
	cases := []struct {
		count int64
		want  string
	}{
		{0, "0 B"},
		{-1536, "-1536 B"},
		{512, "512 B"},
		{1 << 10, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1<<20 - 1, "1.0 MiB"},
		{1 << 20, "1.0 MiB"},
		{3 << 30, "3.0 GiB"},
		{5 << 40, "5.0 TiB"},
		{2 << 50, "2.0 PiB"},
		{1 << 60, "1.0 EiB"},
		{math.MaxInt64, "8.0 EiB"},
	}
	for _, test := range cases {
		if got := Humanize(test.count); got != test.want {
			t.Errorf("Humanize(%d) = %q, want %q", test.count, got, test.want)
		}
	}
}

func ExampleParse() {
	count, err := Parse("1.5 KiB")
	fmt.Println(count, err)
	// Output: 1536 <nil>
}

func ExampleHumanize() {
	fmt.Println(Humanize(1536))
	// Output: 1.5 KiB
}
