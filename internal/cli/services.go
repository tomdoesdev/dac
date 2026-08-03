package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/bytesize"
	"github.com/tom/dac/internal/cache"
	"github.com/tom/dac/internal/credential"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/httpclient"
	"github.com/tom/dac/internal/progress"
	"github.com/tom/dac/internal/rewrite"
)

func (runner *runner) projectService(current *urfave.Command) *application.Service {
	return application.New(current.String("manifest"), current.String("lock"), nil, nil, nil)
}

func (runner *runner) storeService(current *urfave.Command) (*application.Service, error) {
	root, err := cache.ResolveRoot(current.String("cache-dir"))
	if err != nil {
		return nil, fault.Wrap("cache_root_unresolved", "DAC could not resolve the cache directory.", err)
	}
	return application.New(current.String("manifest"), current.String("lock"), cache.New(root), nil, nil), nil
}

// networkService builds one application service for a network command.
// suppressProgress disables progress regardless of --progress, which JSON mode
// needs so that pull writes exactly one document to standard output.
func (runner *runner) networkService(current *urfave.Command, suppressProgress bool) (*application.Service, *httpclient.Client, error) {
	service, err := runner.storeService(current)
	if err != nil {
		return nil, nil, err
	}
	retries := current.Int("retries")
	if retries < 0 {
		return nil, nil, fault.New("invalid_arguments", "The retry count must not be negative.")
	}
	timeout := current.Duration("timeout")
	if timeout <= 0 {
		return nil, nil, fault.New("invalid_arguments", "The timeout must be positive.")
	}
	rewriter, err := loadRewriteConfig(service.ManifestPath, current.Bool("no-rewrite"))
	if err != nil {
		return nil, nil, err
	}
	credentials, err := credential.New(current.StringSlice("credential-helper"), credentialTimeout)
	if err != nil {
		return nil, nil, fault.Wrap("invalid_arguments", "A credential helper is invalid.", err)
	}
	parts := current.Int("download-parts")
	if parts < 1 {
		return nil, nil, fault.New("invalid_arguments", "The download part count must be at least 1.")
	}
	client := httpclient.New(httpclient.Options{
		Timeout:     timeout,
		Retries:     retries,
		Parallelism: parts,
		Rewriter:    rewriter,
		Credentials: credentials,
	})
	service.Fetcher = client
	progressEnabled := current.Bool("progress") && !suppressProgress
	service.Reporter = progress.New(runner.stderr, isTerminal(runner.stderr), progressEnabled)
	return service, client, nil
}

// credentialTimeout bounds one credential helper run. It is deliberately not
// tied to --timeout: that limit covers a multi-gigabyte transfer, and a helper
// that has not answered in half a minute is stuck.
const credentialTimeout = 30 * time.Second

const rewriteConfigName = "dac-rewrite.cfg"

// loadRewriteConfig loads an environment override or the config beside the
// manifest. A missing project config is optional.
func loadRewriteConfig(manifestPath string, disabled bool) (*rewrite.Config, error) {
	if disabled {
		return nil, nil
	}
	path := filepath.Join(filepath.Dir(manifestPath), rewriteConfigName)
	required := false
	if override := os.Getenv("DAC_REWRITE_CONFIG"); override != "" {
		path, required = override, true
	}
	config, err := rewrite.Load(path)
	if err != nil {
		return nil, fault.Wrap("rewrite_config_invalid", "The rewrite config is invalid.", err)
	}
	if config == nil && required {
		return nil, fault.New("rewrite_config_missing", "The rewrite config does not exist.")
	}
	return config, nil
}

func (runner *runner) networkFlags(withConcurrency, withMaxSize bool) []urfave.Flag {
	flags := []urfave.Flag{
		&urfave.DurationFlag{Name: "timeout", Value: 5 * time.Minute, Sources: urfave.EnvVars("DAC_TIMEOUT"), Usage: "Set the transfer inactivity limit."},
		&urfave.IntFlag{Name: "retries", Value: 2, Sources: urfave.EnvVars("DAC_RETRIES"), Usage: "Set the retry count."},
		// The part count is a budget for the whole command rather than a
		// per-asset multiplier, so raising it speeds up a project of one large
		// asset without opening --concurrency times as many connections for a
		// project of many.
		&urfave.IntFlag{Name: "download-parts", Value: 4, Sources: urfave.EnvVars("DAC_DOWNLOAD_PARTS"), Usage: "Set the number of ranged requests one download may be split across."},
		&urfave.BoolFlag{Name: "progress", Value: true, Usage: "Write transfer progress to standard error."},
		&urfave.BoolFlag{Name: "no-rewrite", Usage: "Disable URL rewrite and host policy rules."},
		&urfave.StringSliceFlag{Name: "credential-helper", Sources: urfave.EnvVars("DAC_CREDENTIAL_HELPER"), Usage: "Ask this helper for request headers, as <command> or <host>=<command>."},
	}
	if withConcurrency {
		flags = append(flags, &urfave.IntFlag{Name: "concurrency", Value: 4, Sources: urfave.EnvVars("DAC_CONCURRENCY"), Usage: "Set the number of concurrent assets."})
	}
	if withMaxSize {
		// The limit only ever applies to a response whose size DAC does not know
		// ahead of time, so it exists to stop a runaway stream rather than to
		// express a policy about asset sizes. The old 32GiB default never fired
		// in practice, which made it a knob that generated questions instead of
		// protection. A project with genuinely larger assets raises it.
		flags = append(flags, &urfave.StringFlag{Name: "max-size", Value: "2GiB", Sources: urfave.EnvVars("DAC_MAX_SIZE"), Usage: "Bound a download whose size the lock file does not already give, or none for no bound."})
	}
	return flags
}

// noSizeLimit is the only value that turns the size limit off.
//
// An empty value and a zero used to do it too, and neither said so. Both are
// what a shell produces from a variable nobody set, so the guard against a
// runaway stream could end up disabled by a deployment script that thought it
// was leaving the default in place. Switching it off is a decision, so it needs
// a word.
const noSizeLimit = "none"

func maximumSize(current *urfave.Command) (int64, error) {
	value := strings.TrimSpace(current.String("max-size"))
	if strings.EqualFold(value, noSizeLimit) {
		return 0, nil
	}
	size, err := bytesize.Parse(value)
	if err != nil {
		return 0, fault.Wrap("invalid_arguments", "The maximum size is invalid.", err)
	}
	if size <= 0 {
		return 0, fault.New("invalid_arguments", "The maximum size must be positive. Use --max-size none to download without a limit.")
	}
	return size, nil
}

func concurrency(current *urfave.Command) (int, error) {
	value := current.Int("concurrency")
	if value < 1 {
		return 0, fault.New("invalid_arguments", "The concurrency must be at least 1.")
	}
	return value, nil
}

func isTerminal(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
