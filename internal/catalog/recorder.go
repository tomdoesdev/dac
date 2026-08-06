package catalog

import (
	"context"
	"sync"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
)

// Recorder collects what one run learned about the cache and writes it once.
//
// Records are collected in memory and written at the end, for the reason the trusted-hosts use
// times are: a write per observation would mean one file rewrite per asset of every command that
// reads a project.
type Recorder struct {
	store *Store

	mutex sync.Mutex
	// working is the catalog as this run sees it: what the file held when the run started, plus
	// everything the run has recorded since. Describe answers from it, so a listing can name an
	// object the same command just downloaded.
	working Catalog
	// The changes this run has to write, kept apart from working because the file may have moved
	// under it since, and the merge has to happen against the file rather than against this copy.
	noted     []Entry
	forgotten []string
}

// NewRecorder builds the record for one run from the catalog it loaded.
func NewRecorder(store *Store, loaded Catalog) *Recorder {
	return &Recorder{store: store, working: loaded}
}

// Note records what a run learned. It writes nothing.
func (recorder *Recorder) Note(entries []application.CatalogEntry) {
	if len(entries) == 0 {
		return
	}
	converted := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		converted = append(converted, Entry{
			Digest:     entry.Digest,
			Size:       entry.Size,
			Coordinate: entry.Coordinate,
			URL:        entry.URL,
			Filename:   entry.Filename,
		})
	}
	now := time.Now().UTC()
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	recorder.noted = append(recorder.noted, converted...)
	recorder.working = recorder.working.Note(converted, now)
}

// Forget drops the record of each object that has left the cache.
func (recorder *Recorder) Forget(digests []string) {
	if len(digests) == 0 {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	recorder.forgotten = append(recorder.forgotten, digests...)
	recorder.working, _ = recorder.working.Forget(digests)
}

// Describe reports what DAC remembers one object to be.
func (recorder *Recorder) Describe(value string) (application.CatalogRecord, bool) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	object, held := recorder.working.Objects[value]
	if !held {
		return application.CatalogRecord{}, false
	}
	record := application.CatalogRecord{KnownAs: object.Coordinates(), FirstSeen: object.FirstSeen}
	if newest, found := object.Newest(); found {
		record.Filename, record.SourceURL = newest.Filename, newest.URL
	}
	return record, true
}

// Flush writes what this run learned.
//
// Records are applied before removals, which is the order they happened in: a targeted removal
// reads the project it is removing from, so the assets it named are recorded and then dropped.
// The other order would put every one of them back.
func (recorder *Recorder) Flush(ctx context.Context) error {
	noted, forgotten := recorder.drain()
	if len(noted) == 0 && len(forgotten) == 0 {
		return nil
	}
	now := time.Now().UTC()
	_, err := recorder.store.Update(ctx, func(current Catalog) (Catalog, error) {
		updated, _ := current.Note(noted, now).Forget(forgotten)
		return updated, nil
	})
	return err
}

// drain takes the changes recorded so far.
func (recorder *Recorder) drain() ([]Entry, []string) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	noted, forgotten := recorder.noted, recorder.forgotten
	recorder.noted, recorder.forgotten = nil, nil
	return noted, forgotten
}
