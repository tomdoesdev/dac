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
	"github.com/tomdoesdev/dac/internal/trust"
)

// projectPaths returns the manifest and lock file this run acts on.
// The lock file follows the manifest unless it was named.
func projectPaths(current *urfave.Command) (string, string) {
	manifest := current.String("manifest")
	if current.IsSet("lock") {
		return manifest, current.String("lock")
	}
	return manifest, filepath.Join(filepath.Dir(manifest), DefaultLock)
}

func (runner *runner) projectService(current *urfave.Command) *application.Service {
	manifest, lock := projectPaths(current)
	service := application.New(manifest, lock, nil, nil, nil)
	runner.attachCatalog(current, service)
	return service
}

// attachCatalog gives a service the object catalog to record into, when there is one.
//
// The nil check is on the concrete recorder rather than after the assignment, because a nil
// pointer stored in an interface field is not a nil interface: every guard in the service would
// pass and every call would land on a nil receiver.
//
// The catalog is attached to every service, including the ones holding no object store. Reading
// a project is what advances a record, and remove does that without ever opening the
// cache.
func (runner *runner) attachCatalog(current *urfave.Command, service *application.Service) {
	if recorder := runner.catalogRecorder(current); recorder != nil {
		service.Catalog = recorder
	}
}

func (runner *runner) storeService(current *urfave.Command) (*application.Service, error) {
	root, err := runner.cacheRoot(current)
	if err != nil {
		return nil, err
	}
	// The trust list is read here rather than only by the commands that download,
	// because info reports what a download would do without making one.
	_, list, err := runner.trustFile(current)
	if err != nil {
		return nil, err
	}
	manifest, lock := projectPaths(current)
	store := cache.New(root)
	store.Logger = runner.trace(current)
	service := application.New(manifest, lock, store, nil, nil)
	service.Trust = list
	runner.attachCatalog(current, service)
	return service, nil
}

// trace returns the logger for this run.
func (runner *runner) trace(current *urfave.Command) *slog.Logger {
	return debug.New(runner.stderr, current.Bool("debug"))
}

// cacheRoot resolves the cache directory from the flag, then the config file, then the XDG cache location.
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
// The command context controls transfers and their progress reporter together.
func (runner *runner) networkService(ctx context.Context, current *urfave.Command, suppressProgress bool) (*application.Service, *httpclient.Client, error) {
	settings, err := runner.config(current)
	if err != nil {
		return nil, nil, err
	}
	service, err := runner.storeService(current)
	if err != nil {
		return nil, nil, err
	}
	store, list, err := runner.trustFile(current)
	if err != nil {
		return nil, nil, err
	}
	// The refusal is a transport stage because that is the one place every request
	// passes through: the asset, each redirect it follows, and each range of a split.
	runner.gate = trust.NewGate(store, list, current.Bool("insecure-trust-all"))
	trace := runner.trace(current)
	client := httpclient.New(httpclient.Options{
		Timeout:             settings.Timeout,
		Retries:             settings.Retries,
		Parallelism:         settings.DownloadParts,
		TransportDecorators: []httpclient.TransportDecorator{runner.gate.Decorate},
		Logger:              trace,
	})
	service.Fetcher = client
	service.Prober = client
	// A trace and a progress bar share standard error, and mpb redraws in place.
	progressEnabled := settings.Progress && !current.Bool("no-progress") && !suppressProgress && !current.Bool("debug")
	service.Reporter = progress.New(ctx, runner.stderr, isTerminal(runner.stderr), progressEnabled, runner.stderrPalette)
	return service, client, nil
}

// networkFlags returns the request options that a user can select for one run.
func (runner *runner) networkFlags(withConcurrency bool) []urfave.Flag {
	flags := []urfave.Flag{
		&urfave.BoolFlag{Name: "no-progress", Usage: "Do not write transfer progress to standard error."},
		// This flag takes no environment variable on purpose: a variable that turns
		// off a refusal turns it off for everything a shell later runs, silently.
		&urfave.BoolFlag{Name: "insecure-trust-all", Usage: "Download from any host, ignoring the trusted-hosts file."},
	}
	if withConcurrency {
		flags = append(flags, &urfave.IntFlag{
			Name:    "concurrency",
			Sources: urfave.EnvVars("DAC_CONCURRENCY"),
			Usage:   "Set the number of concurrent assets.",
			// The flag carries no value of its own, so urfave would advertise the zero one.
			DefaultText: "transfer.concurrency, or " + strconv.Itoa(config.DefaultConcurrency),
		})
	}
	return flags
}

// flagOrConfig reads one setting that a command flag may override for a single run.
// The flag is parsed with the function that parses the same setting out of the config file, so a
// value is accepted or refused identically whichever way it arrives, and the rule that a flag beats
// the file is stated once. It is a function rather than a method because Go methods take no type
// parameters. Settings with no flag, or whose flag is not a parsed string, read the config directly.
func flagOrConfig[T any](runner *runner, current *urfave.Command, flag, invalid string,
	parse func(string) (T, error), pick func(*config.Config) T,
) (T, error) {
	var zero T
	if current.IsSet(flag) {
		value, err := parse(current.String(flag))
		if err != nil {
			return zero, fault.Wrap("invalid_arguments", invalid, err)
		}
		return value, nil
	}
	settings, err := runner.config(current)
	if err != nil {
		return zero, err
	}
	return pick(settings), nil
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
