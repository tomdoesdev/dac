package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/cache"
	"github.com/tomdoesdev/dac/internal/config"
	"github.com/tomdoesdev/dac/internal/credential"
	"github.com/tomdoesdev/dac/internal/debug"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/httpclient"
	"github.com/tomdoesdev/dac/internal/progress"
	"github.com/tomdoesdev/dac/internal/rewrite"
)

// projectPaths returns the manifest and lock file this run acts on.
//
// The lock file follows the manifest unless it was named. A project is its two
// files together, and pointing --manifest at another directory used to leave
// the lock behind in the working directory, which produced a pair that did not
// describe each other and no message saying so. The rewrite config already
// resolves beside the manifest; this makes the lock agree.
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
	rewriter, err := loadRewriteConfig(settings, service.ManifestPath, current.Bool("no-rewrite"))
	if err != nil {
		return nil, nil, err
	}
	credentials, err := credential.New(settings.Credentials, credentialTimeout)
	if err != nil {
		return nil, nil, fault.Wrap("config_invalid", "A credential helper is invalid.", err)
	}
	trace := runner.trace(current)
	if credentials != nil {
		credentials.Logger = trace
	}
	client := httpclient.New(httpclient.Options{
		Timeout:     settings.Timeout,
		Retries:     settings.Retries,
		Parallelism: settings.DownloadParts,
		Rewriter:    rewriter,
		Credentials: credentials,
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

// credentialTimeout bounds one credential helper run. It is deliberately not
// tied to --timeout: that limit covers a multi-gigabyte transfer, and a helper
// that has not answered in half a minute is stuck.
const credentialTimeout = 30 * time.Second

const rewriteConfigName = "dac-rewrite.cfg"

// loadRewriteConfig chooses the rewrite rules for one run.
//
// A rewrite config answers "where does this site actually send its downloads",
// which is a property of the machine and not of the project -- it exists so a
// site that proxies its downloads does not have to edit every repository that
// uses them. So the config file is where it belongs, and DAC finds it through
// the XDG search path like everything else.
//
// A dac-rewrite.cfg beside the manifest still wins outright. A project that
// carries its own rules is saying something about itself rather than adding to
// what the site said, and merging the two would produce a policy neither
// wrote.
func loadRewriteConfig(settings *config.Config, manifestPath string, disabled bool) (*rewrite.Config, error) {
	if disabled {
		return nil, nil
	}
	path := filepath.Join(filepath.Dir(manifestPath), rewriteConfigName)
	project, err := rewrite.Load(path)
	if err != nil {
		return nil, fault.Wrap("rewrite_config_invalid", "The rewrite config is invalid.", err)
	}
	if project != nil {
		return project, nil
	}
	return settings.Rewrite, nil
}

// networkFlags returns the options a command that makes requests still carries.
//
// What is left is what a person decides for one run. How long to wait, how hard
// to retry, how many pieces a download may be split into, how large a response
// may get, and which helper answers for which host are properties of the
// machine, so they moved into the config file: they were on the command line
// because there was nowhere else to put them, and the cost was that four
// commands each had to carry all six.
func (runner *runner) networkFlags(withConcurrency bool) []urfave.Flag {
	flags := []urfave.Flag{
		&urfave.BoolFlag{Name: "no-progress", Usage: "Do not write transfer progress to standard error."},
		&urfave.BoolFlag{Name: "no-rewrite", Usage: "Disable URL rewrite and host policy rules."},
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
