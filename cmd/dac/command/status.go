package command

import (
	"context"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/lockfile"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
)

type statusCommand struct{ runtime *runtime }

func (*statusCommand) Description() string { return "Inspect lock and downloaded asset state" }
func (command *statusCommand) Validate() error {
	return command.runtime.Output.ValidateOptions()
}

func (command *statusCommand) Run(ctx context.Context) error {
	paths, err := command.runtime.project()
	if err != nil {
		return err
	}
	return paths.WithLock(ctx, func(context.Context) error {
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			return err
		}
		resolved, err := manifest.Resolve(value)
		if err != nil {
			return err
		}
		locked, exists, err := lockfile.LoadOptional(paths.Lockfile())
		if err != nil {
			return err
		}
		if !exists {
			locked = lockfile.Lockfile{Version: lockfile.Version, Files: map[string]lockfile.Asset{}}
		}
		evaluation, err := lockfile.Evaluate(resolved, locked)
		if err != nil {
			return err
		}
		downloads, hasDownloads, err := paths.OpenDownloadsOptional()
		if err != nil {
			return err
		}
		if downloads != nil {
			defer func() { _ = downloads.Close() }()
		}
		results, err := statusResults(downloads, hasDownloads, evaluation)
		if err != nil {
			return err
		}
		orphans, err := statusOrphans(downloads, hasDownloads, locked, evaluation)
		if err != nil {
			return err
		}
		return command.runtime.Output.Status(paths.Root, results, orphans)
	})
}

// statusResults combines configuration currentness with local byte integrity
// without making network requests or changing the downloads directory.
func statusResults(downloads *project.Downloads, hasDownloads bool, evaluation lockfile.Evaluation) ([]output.Result, error) {
	results := make([]output.Result, 0, len(evaluation.Entries))
	for _, entry := range evaluation.Entries {
		if entry.State == lockfile.StateStale {
			results = append(results, output.Result{Name: entry.Resolved.Name, Status: "stale", File: entry.Resolved.ResolvedFile, Reason: entry.Reason})
			continue
		}
		if !hasDownloads {
			results = append(results, output.Result{Name: entry.Resolved.Name, Status: "missing", File: entry.Locked.ResolvedFile, Digest: entry.Locked.Digest, Size: entry.Locked.Size, Reason: "download is missing"})
			continue
		}
		inspection, err := asset.InspectLocal(downloads.Root(), entry.Locked.ResolvedFile, entry.Locked.Digest, entry.Locked.Size)
		if err != nil {
			return nil, err
		}
		results = append(results, output.Result{
			Name: entry.Resolved.Name, Status: string(inspection.State), File: entry.Locked.ResolvedFile,
			Digest: entry.Locked.Digest, Size: entry.Locked.Size, Reason: inspection.Reason,
		})
	}
	return results, nil
}

// statusOrphans reports lock-only assets separately from untracked directory
// entries so callers can decide which kind of cleanup is appropriate. Reading
// the directory belongs to the package that owns it.
func statusOrphans(downloads *project.Downloads, hasDownloads bool, locked lockfile.Lockfile, evaluation lockfile.Evaluation) ([]output.Orphan, error) {
	orphans := make([]output.Orphan, 0, len(evaluation.Orphans))
	for _, name := range evaluation.Orphans {
		file := locked.Files[name]
		orphans = append(orphans, output.Orphan{Kind: "lock", Name: name, File: file.ResolvedFile, Reason: "asset is absent from dac.toml"})
	}
	if !hasDownloads {
		return orphans, nil
	}
	unreferenced, err := downloads.Unreferenced(locked.ResolvedFiles())
	if err != nil {
		return nil, err
	}
	for _, name := range unreferenced {
		orphans = append(orphans, output.Orphan{Kind: "file", File: name, Reason: "download entry is not referenced by dac.lock"})
	}
	return orphans, nil
}
