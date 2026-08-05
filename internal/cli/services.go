package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/cache"
	"github.com/tomdoesdev/dac/internal/config"
	"github.com/tomdoesdev/dac/internal/debug"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/httpclient"
	"github.com/tomdoesdev/dac/internal/progress"
)

// projectPaths returns the manifest and lock file this run acts on.
//
// The lock file follows the manifest unless it was named. A project is its two
// files together. The lock follows an explicitly selected manifest.
func projectPaths(current *urfave.Command) (string, string) {
	manifest := current.String("manifest")
	if current.IsSet("lock") {
		return manifest, current.String("lock")
	}
	return manifest, filepath.Join(filepath.Dir(manifest), DefaultLock)
}

func (runner *runner) projectService(current *urfave.Command) *application.Service {
	manifest, lock := projectPaths(current)
	return application.New(manifest, lock, nil, nil, nil)
}

func (runner *runner) storeService(current *urfave.Command) (*application.Service, error) {
	root, err := runner.cacheRoot(current)
	if err != nil {
		return nil, err
	}
	manifest, lock := projectPaths(current)
	store := cache.New(root)
	store.Logger = runner.trace(current)
	return application.New(manifest, lock, store, nil, nil), nil
}

// trace returns the logger for this run. It writes to standard error, where
// help and progress already go, because standard output carries the output
// contract and a trace is not a command result.
func (runner *runner) trace(current *urfave.Command) *slog.Logger {
	return debug.New(runner.stderr, current.Bool("debug"))
}

// cacheRoot resolves the cache directory from the flag, then the config file,
// then the XDG cache location.
func (runner *runner) cacheRoot(current *urfave.Command) (string, error) {
	selected := current.String("cache-dir")
	if selected == "" {
		settings, err := runner.config(current)
		if err != nil {
			return "", err
		}
		selected = settings.CacheDir
	}
	root, err := cache.ResolveRoot(selected)
	if err != nil {
		return "", fault.Wrap("cache_root_unresolved", "DAC could not resolve the cache directory.", err)
	}
	return root, nil
}

// networkService builds one application service for a network command.
// suppressProgress disables progress regardless of --progress, which JSON mode
// needs so that pull writes exactly one document to standard output.
//
// The context is the command's, and it reaches the progress reporter as well as
// the transfers: an interrupt has to end the display along with the downloads
// it was following.
func (runner *runner) networkService(ctx context.Context, current *urfave.Command, suppressProgress bool) (*application.Service, *httpclient.Client, error) {
	settings, err := runner.config(current)
	if err != nil {
		return nil, nil, err
	}
	service, err := runner.storeService(current)
	if err != nil {
		return nil, nil, err
	}
	trace := runner.trace(current)
	client := httpclient.New(httpclient.Options{
		Timeout:     settings.Timeout,
		Retries:     settings.Retries,
		Parallelism: settings.DownloadParts,
		Logger:      trace,
	})
	service.Fetcher = client
	// A trace and a progress bar share standard error, and mpb redraws in
	// place. Two writers to one terminal produce a display that is neither, so
	// asking what happened turns the bars off the way JSON mode does.
	progressEnabled := settings.Progress && !current.Bool("no-progress") && !suppressProgress && !current.Bool("debug")
	service.Reporter = progress.New(ctx, runner.stderr, isTerminal(runner.stderr), progressEnabled, runner.stderrPalette)
	return service, client, nil
}

// networkFlags returns the request options that a user can select for one run.
// Stable transfer limits stay in the config file so commands share one policy.
func (runner *runner) networkFlags(withConcurrency bool) []urfave.Flag {
	flags := []urfave.Flag{
		&urfave.BoolFlag{Name: "no-progress", Usage: "Do not write transfer progress to standard error."},
	}
	if withConcurrency {
		flags = append(flags, &urfave.IntFlag{
			Name:    "concurrency",
			Sources: urfave.EnvVars("DAC_CONCURRENCY"),
			Usage:   "Set the number of concurrent assets.",
			// The flag carries no value of its own, so urfave would advertise
			// the zero one. What it actually falls back to is the config file,
			// and saying so is the only way somebody finds out where to set it.
			DefaultText: "transfer.concurrency, or " + strconv.Itoa(config.DefaultConcurrency),
		})
	}
	return flags
}

// concurrency reads the asset parallelism, preferring the flag over the config.
func (runner *runner) concurrency(current *urfave.Command) (int, error) {
	if current.IsSet("concurrency") {
		value := current.Int("concurrency")
		if value < 1 {
			return 0, fault.New("invalid_arguments", "The concurrency must be at least 1.")
		}
		return value, nil
	}
	settings, err := runner.config(current)
	if err != nil {
		return 0, err
	}
	return settings.Concurrency, nil
}

// maximumSize reads the download bound from the config.
func (runner *runner) maximumSize(current *urfave.Command) (int64, error) {
	settings, err := runner.config(current)
	if err != nil {
		return 0, err
	}
	return settings.MaxSize, nil
}

func isTerminal(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
