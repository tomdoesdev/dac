package bytesize

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		value string
		want  int64
	}{
		{"0", 0},
		{"512", 512},
		{"1B", 1},
		{"8KiB", 8192},
		{"1MiB", 1 << 20},
		{"2GiB", 2 << 30},
		{"1TiB", 1 << 40},
		{"1KB", 1000},
		{"1MB", 1000 * 1000},
		{"1GB", 1000 * 1000 * 1000},
		{"1TB", 1000 * 1000 * 1000 * 1000},
		{"1K", 1 << 10},
		{"1M", 1 << 20},
		{"1G", 1 << 30},
		{"1T", 1 << 40},
		{" 1.5MiB ", 1572864},
		{"1.5kib", 1536},
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
	for _, value := range []string{"", "  ", "-1", "-1MiB", "MiB", "1XiB", "one", "1MiB/s"} {
		if got, err := Parse(value); err == nil {
			t.Errorf("Parse(%q) = %d, want an error", value, got)
		}
	}
}

func TestFormatUsesBinaryUnits(t *testing.T) {
	for _, testCase := range []struct {
		count    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1 << 10, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{3 << 30, "3.0 GiB"},
		{5 << 40, "5.0 TiB"},
		{2 << 50, "2.0 PiB"},
	} {
		if got := Format(testCase.count); got != testCase.expected {
			t.Fatalf("Format(%d) = %q, want %q", testCase.count, got, testCase.expected)
		}
	}
}
