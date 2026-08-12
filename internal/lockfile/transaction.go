package lockfile

import (
	"errors"
	"io/fs"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/kit/fs/atomic"
)

// Transaction installs downloaded artifacts and the lock metadata describing
// them as one reversible unit, so a caller's control flow cannot skip rollback
// or cleanup on a single branch.
type Transaction struct {
	downloads map[string]*asset.StagedDownload
	lock      *atomic.File
}

// NewTransaction starts an empty transaction. Callers must defer Discard.
func NewTransaction() *Transaction {
	return &Transaction{downloads: make(map[string]*asset.StagedDownload)}
}

// Add enrolls verified bytes staged for one asset.
func (transaction *Transaction) Add(name string, download *asset.StagedDownload) {
	transaction.downloads[name] = download
}

// Stage prepares the lock file that will replace accepted state.
func (transaction *Transaction) Stage(path string, value Lockfile) error {
	lock, err := Stage(path, value)
	if err != nil {
		return err
	}
	transaction.lock = lock
	return nil
}

// Discard releases every staged artifact and the staged lock file. It is safe
// to call after a successful Commit.
func (transaction *Transaction) Discard() {
	for _, download := range transaction.downloads {
		_ = download.Discard()
	}
	if transaction.lock != nil {
		_ = transaction.lock.Discard()
	}
}

// Commit installs downloaded assets before atomically installing their lock
// metadata, rolling the whole operation back when any single commit fails. It
// returns warnings for recovery files that survived an otherwise successful
// commit. When createOnly is set the lock must not already exist, which closes
// the race between an earlier existence check and this install.
func (transaction *Transaction) Commit(order []string, createOnly bool) ([]string, error) {
	commits := make([]*atomic.Commit, 0, len(transaction.downloads)+1)
	for _, name := range order {
		download := transaction.downloads[name]
		if download == nil {
			continue
		}
		commit, err := download.CommitReversible()
		if commit != nil {
			commits = append(commits, commit)
		}
		if err != nil {
			return nil, rollback(commits, fault.NewFilesystemError(err))
		}
	}
	commit, err := transaction.commitLock(createOnly)
	if commit != nil {
		commits = append(commits, commit)
	}
	if err != nil {
		if createOnly && errors.Is(err, fs.ErrExist) {
			return nil, rollback(commits, fault.NewConfigurationError(ErrAlreadyExists))
		}
		return nil, rollback(commits, fault.NewFilesystemError(err))
	}
	warnings := make([]string, 0)
	for _, commit := range commits {
		if err := commit.Complete(); err != nil {
			warnings = append(warnings, "could not remove transaction recovery file: "+err.Error())
		}
	}
	return warnings, nil
}

func (transaction *Transaction) commitLock(createOnly bool) (*atomic.Commit, error) {
	if createOnly {
		return transaction.lock.CommitNoReplace()
	}
	return transaction.lock.CommitReversible()
}

// rollback reverses completed commits in reverse order and joins every failure
// onto the original cause.
func rollback(commits []*atomic.Commit, original error) error {
	cleanup := make([]error, 0, len(commits)+1)
	cleanup = append(cleanup, original)
	for index := len(commits) - 1; index >= 0; index-- {
		cleanup = append(cleanup, commits[index].Rollback())
	}
	return errors.Join(cleanup...)
}
