package cache

// This file implements the sidecar that makes the object store tamper-evident.
// A content-addressed store names an object after its bytes, which makes that name a claim rather than a fact.
// So each object carries a sidecar recording the size and modification time it had when DAC installed it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/jsonfile"
)

const (
	metaVersion  = 1
	metaSuffix   = ".meta"
	metaFileMode = 0o644
)

// meta describes the object DAC installed, as it was at the moment of install.
type meta struct {
	MetaVersion int   `json:"metaVersion"`
	Size        int64 `json:"size"`
	ModifiedAt  int64 `json:"modifiedAt"`
}

func newMeta(info os.FileInfo) meta {
	return meta{MetaVersion: metaVersion, Size: info.Size(), ModifiedAt: info.ModTime().UnixNano()}
}

// describes reports whether a sidecar still matches the object it was written for.
func (value meta) describes(info os.FileInfo) bool {
	return value.MetaVersion == metaVersion &&
		value.Size == info.Size() &&
		value.ModifiedAt == info.ModTime().UnixNano()
}

func metaPath(objectPath string) string { return objectPath + metaSuffix }

// readMeta reads one sidecar.
func readMeta(path string) (meta, bool, error) {
	var value meta
	err := jsonfile.ReadStrict(path, &value)
	if errors.Is(err, os.ErrNotExist) {
		return meta{}, false, nil
	}
	// A sidecar DAC cannot parse is a sidecar DAC will rewrite.
	if err != nil {
		return meta{}, false, nil
	}
	return value, true, nil
}

func writeMeta(path string, value meta) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return jsonfile.WriteAtomic(path, data, metaFileMode)
}

// check confirms that an object still holds the bytes DAC installed.
// The cheap path is a stat compared against the sidecar.
func (store *Store) check(value, path string, info os.FileInfo) error {
	if store.trusted(value, info) {
		return nil
	}
	record, found, err := readMeta(metaPath(path))
	if err != nil {
		return err
	}
	if found && record.describes(info) {
		store.remember(value, info)
		return nil
	}
	return store.verify(value, path)
}

// verify hashes an object and either records what it found or reports the object as corrupt.
func (store *Store) verify(value, path string) error {
	actual, info, err := hashFile(path)
	if err != nil {
		return err
	}
	if actual != value {
		return &application.CorruptError{Digest: value, ActualDigest: actual, Path: path}
	}
	// The object is what it claims to be, so record the stat that proves it.
	_ = writeMeta(metaPath(path), newMeta(info))
	store.remember(value, info)
	return nil
}

// hashFile hashes an object and reports the file information for the exact bytes it read.
// That information comes from the open descriptor rather than from the path, because an install renames a new object over the old one.
func hashFile(path string) (string, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = file.Close() }()
	hashValue := sha256.New()
	if _, err := io.Copy(hashValue, file); err != nil {
		return "", nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	return digest.Prefix + hex.EncodeToString(hashValue.Sum(nil)), info, nil
}

// record writes the sidecar for a freshly installed object.
// A failure here is fatal even though the object itself is already in place.
func (store *Store) record(value, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := writeMeta(metaPath(path), newMeta(info)); err != nil {
		return err
	}
	store.remember(value, info)
	return nil
}

// trusted reports whether this process has already hashed these exact bytes.
// Process-local trust prevents repeated hashes of one unchanged object.
func (store *Store) trusted(value string, info os.FileInfo) bool {
	record, found := store.verified.Load(value)
	return found && record.(meta).describes(info)
}

func (store *Store) remember(value string, info os.FileInfo) {
	store.verified.Store(value, newMeta(info))
}

// touch refreshes a sidecar timestamp so collection can tell an object that is still in use from one that no project has referenced in a long time.
// It deliberately leaves the object alone.
func touch(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}
