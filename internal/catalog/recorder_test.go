package catalog

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/dac"
	"github.com/tomdoesdev/dac/internal/digest"
)

func testRecorder(t *testing.T) *Recorder {
	t.Helper()
	return NewRecorder(testStore(t), Empty())
}

func observation(value, name string) dac.CatalogEntry {
	return dac.CatalogEntry{
		Digest:     value,
		Size:       12,
		Coordinate: coord.MustParse(name),
		URL:        "https://example.test/a",
		Filename:   "a.bin",
	}
}

// TestFlushWritesNothingWhenTheRunSawNothing keeps commands that never open a project, like
// a cache listing, from touching the file at all.
func TestFlushWritesNothingWhenTheRunSawNothing(t *testing.T) {
	recorder := testRecorder(t)
	if err := recorder.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := os.Stat(recorder.store.Path); !os.IsNotExist(err) {
		t.Fatalf("Stat = %v, want no file at all", err)
	}
}

func TestFlushRecordsEveryObjectTheRunSaw(t *testing.T) {
	recorder := testRecorder(t)
	recorder.Note([]dac.CatalogEntry{
		observation(first, "tools/rg@14.1.0"),
		observation(second, "tools/jq@1.7"),
	})
	if err := recorder.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	loaded, err := recorder.store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Objects) != 2 {
		t.Fatalf("catalog = %#v, want both objects", loaded)
	}
}

// TestFlushAppliesRecordsBeforeRemovals is the order these happened in. A targeted removal reads
// the project it is removing from, so the assets it named are recorded and then dropped. The
// other order would put every one of them straight back.
func TestFlushAppliesRecordsBeforeRemovals(t *testing.T) {
	recorder := testRecorder(t)
	recorder.Note([]dac.CatalogEntry{observation(first, "tools/rg@14.1.0")})
	recorder.Forget([]string{first})
	if err := recorder.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	loaded, err := recorder.store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Objects) != 0 {
		t.Fatalf("catalog = %#v, want the removed object forgotten", loaded)
	}
}

// TestDescribeAnswersFromWhatThisRunHasSeen is what lets one command download an object and then
// name it in the same listing, before anything has been written.
func TestDescribeAnswersFromWhatThisRunHasSeen(t *testing.T) {
	recorder := testRecorder(t)
	recorder.Note([]dac.CatalogEntry{observation(first, "tools/rg@14.1.0")})
	record, known := recorder.Describe(first)
	if !known || record.Filename != "a.bin" || record.SourceURL != "https://example.test/a" {
		t.Fatalf("record = %#v, want the observation this run made", record)
	}
	if _, known := recorder.Describe(second); known {
		t.Fatalf("Describe answered for an object nothing recorded")
	}
}

// TestTheRecorderHoldsUpUnderParallelRecording exists because pull resolves and installs assets
// concurrently, so several goroutines record into one recorder at once.
func TestTheRecorderHoldsUpUnderParallelRecording(t *testing.T) {
	recorder := testRecorder(t)
	const count = 16
	var group sync.WaitGroup
	for index := range count {
		group.Go(func() {
			value := digest.Bytes([]byte("object " + strconv.Itoa(index)))
			recorder.Note([]dac.CatalogEntry{observation(value, "tools/rg@14.1."+strconv.Itoa(index))})
		})
	}
	group.Wait()
	if err := recorder.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	loaded, err := recorder.store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Objects) != count {
		t.Fatalf("catalog holds %d objects, want %d", len(loaded.Objects), count)
	}
}
