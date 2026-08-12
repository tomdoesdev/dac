package atomic

import "testing"

// TestDefaultSettings pins the safety-oriented behavior callers receive without
// options: create parents, use the documented modes and staging prefix, and sync
// data so a successful return includes durability rather than visibility alone.
func TestDefaultSettings(t *testing.T) {
	current := newSettings(nil)
	if current.dirMode != defaultDirMode {
		t.Fatalf("the default directory mode is %v", current.dirMode)
	}
	if !current.makeDirs {
		t.Fatal("the default settings do not create directories")
	}
	if current.tempPrefix != DefaultTempPrefix {
		t.Fatalf("the default prefix is %q", current.tempPrefix)
	}
	if !current.sync {
		t.Fatal("the default settings do not sync")
	}
}

// TestOptionsChangeOnlyTheirOwnField guards option composition. Enabling one
// behavior must not reset an unrelated choice, which lets DAC build settings
// from independent concerns without depending on option order.
func TestOptionsChangeOnlyTheirOwnField(t *testing.T) {
	current := newSettings([]Option{WithDirMode(0o700), WithoutMkdir(), WithTempPrefix(".dac-"), WithoutSync()})
	if current.dirMode != 0o700 {
		t.Fatalf("the directory mode is %v", current.dirMode)
	}
	if current.makeDirs {
		t.Fatal("WithoutMkdir left directory creation on")
	}
	if current.tempPrefix != ".dac-" {
		t.Fatalf("the prefix is %q", current.tempPrefix)
	}
	if current.sync {
		t.Fatal("WithoutSync left syncing on")
	}
}

// A temporary file with no prefix is hard to tell from a real one, so an empty
// prefix is a value to ignore rather than a value to honour.
func TestTempPrefixKeepsTheDefaultForAnEmptyValue(t *testing.T) {
	current := newSettings([]Option{WithTempPrefix("")})
	if current.tempPrefix != DefaultTempPrefix {
		t.Fatalf("the prefix is %q", current.tempPrefix)
	}
}
