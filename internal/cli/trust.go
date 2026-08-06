package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/config"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/style"
	"github.com/tomdoesdev/dac/internal/trust"
)

// trustCommand builds the commands that manage which hosts DAC will download from.
func (runner *runner) trustCommand() *urfave.Command {
	return &urfave.Command{
		Name:            "trust",
		Usage:           "List and change the hosts DAC will download from.",
		HideHelpCommand: true,
		Commands: []*urfave.Command{
			runner.trustListCommand(),
			runner.trustAddCommand(),
			runner.trustRemoveCommand(),
			runner.trustGCCommand(),
			runner.trustPathCommand(),
		},
		Action: runner.helpOrInvalid,
	}
}

func (runner *runner) trustListCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "list",
		Usage: "List the trusted hosts and when each was last used.",
		Action: runner.runBare("trust.list", func(_ context.Context, current *urfave.Command) (any, string, error) {
			store, list, err := runner.trustFile(current)
			if err != nil {
				return nil, "", err
			}
			result := application.TrustListResult{Path: store.Path, Hosts: []application.TrustHost{}}
			for _, host := range list.Sorted() {
				entry := list.Hosts[host]
				result.Hosts = append(result.Hosts, application.TrustHost{
					Host: host, AddedAt: entry.AddedAt, LastUsed: entry.LastUsed,
				})
			}
			return result, trustListText(runner.stdoutPalette, result), nil
		}),
	}
}

func (runner *runner) trustAddCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "add",
		Usage:     "Trust one or more hosts.",
		ArgsUsage: "<host|url>...",
		Action: runner.run("trust.add", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			hosts, err := hostArguments(current)
			if err != nil {
				return nil, "", err
			}
			store, _, err := runner.trustFile(current)
			if err != nil {
				return nil, "", err
			}
			added, err := runner.changeTrust(ctx, store, func(list trust.List) (trust.List, []string) {
				return list.Add(hosts, time.Now().UTC())
			})
			if err != nil {
				return nil, "", err
			}
			result := application.TrustAddResult{Path: store.Path, Added: added, Present: without(hosts, added)}
			return result, trustAddText(runner.stdoutPalette, result), nil
		}),
	}
}

func (runner *runner) trustRemoveCommand() *urfave.Command {
	return &urfave.Command{
		Name:          "remove",
		Usage:         "Withdraw trust from one or more hosts.",
		ArgsUsage:     "<host|url>...",
		ShellComplete: runner.completeHosts(),
		Action: runner.run("trust.remove", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			hosts, err := hostArguments(current)
			if err != nil {
				return nil, "", err
			}
			store, _, err := runner.trustFile(current)
			if err != nil {
				return nil, "", err
			}
			removed, err := runner.changeTrust(ctx, store, func(list trust.List) (trust.List, []string) {
				return list.Remove(hosts)
			})
			if err != nil {
				return nil, "", err
			}
			result := application.TrustRemoveResult{Path: store.Path, Removed: removed, Absent: without(hosts, removed)}
			return result, trustRemoveText(runner.stdoutPalette, result), nil
		}),
	}
}

// trustGCCommand builds the collection that drops hosts nothing has downloaded from.
// A trust list only means something if it is the hosts a project still uses, and
// one that only ever grows stops being a statement about anything.
func (runner *runner) trustGCCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "gc",
		Usage: "Withdraw trust from hosts that nothing has downloaded from recently.",
		Flags: []urfave.Flag{
			&urfave.StringFlag{
				Name:        "max-age",
				Usage:       "Keep hosts used within this period.",
				DefaultText: "trust.max-age, or " + config.DefaultTrustMaxAge,
			},
			&urfave.BoolFlag{Name: "dry-run", Usage: "Report what collection would withdraw."},
		},
		Action: runner.runBare("trust.gc", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			maxAge, err := runner.trustMaximumAge(current)
			if err != nil {
				return nil, "", err
			}
			store, list, err := runner.trustFile(current)
			if err != nil {
				return nil, "", err
			}
			cutoff := time.Now().UTC().Add(-maxAge)
			dryRun := current.Bool("dry-run")
			var collected []string
			if dryRun {
				_, collected = list.Collect(cutoff)
			} else {
				collected, err = runner.changeTrust(ctx, store, func(list trust.List) (trust.List, []string) {
					return list.Collect(cutoff)
				})
				if err != nil {
					return nil, "", err
				}
			}
			result := application.TrustGCResult{
				Path: store.Path, Hosts: collected, HostCount: len(collected), DryRun: dryRun,
			}
			return result, trustGCText(runner.stdoutPalette, result), nil
		}),
	}
}

func (runner *runner) trustPathCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "path",
		Usage: "Print the resolved trusted-hosts file.",
		Action: runner.runBare("trust.path", func(_ context.Context, current *urfave.Command) (any, string, error) {
			store, _, err := runner.trustFile(current)
			if err != nil {
				return nil, "", err
			}
			// The bare path and nothing else, so a script can read it the way it reads dac cache dir.
			return application.TrustPathResult{Path: store.Path}, store.Path, nil
		}),
	}
}

// changeTrust applies one change to the trusted-hosts file under its lock and reports the hosts it touched.
// Replacing the loaded list is the reason this is one method rather than four call sites: networkService
// builds the refusal from runner.trustList, and a caller that wrote the file but left the stale list would
// refuse the host it had just trusted. See trustSource.
func (runner *runner) changeTrust(ctx context.Context, store *trust.Store, change func(trust.List) (trust.List, []string)) ([]string, error) {
	var affected []string
	updated, err := store.Update(ctx, func(list trust.List) (trust.List, error) {
		var changed trust.List
		changed, affected = change(list)
		return changed, nil
	})
	if err != nil {
		return nil, trustWriteError(err)
	}
	runner.trustList = updated
	return affected, nil
}

// trustSource trusts the host of an add's source URL before the request goes out.
// The loaded list is replaced because networkService builds the refusal from it a
// moment later, and a stale one would refuse the add that just asked for this.
func (runner *runner) trustSource(ctx context.Context, current *urfave.Command, source string) error {
	host, err := trust.ParseHost(source)
	if err != nil {
		return fault.Wrap("invalid_arguments", "The source URL names no host to trust.", err)
	}
	store, _, err := runner.trustFile(current)
	if err != nil {
		return err
	}
	_, err = runner.changeTrust(ctx, store, func(list trust.List) (trust.List, []string) {
		return list.Add([]string{host}, time.Now().UTC())
	})
	return err
}

// trustMaximumAge reads the collection age, preferring the flag over the config.
func (runner *runner) trustMaximumAge(current *urfave.Command) (time.Duration, error) {
	return flagOrConfig(runner, current, "max-age", "The maximum age is invalid.", config.ParseDuration,
		func(settings *config.Config) time.Duration { return settings.TrustMaxAge })
}

// hostArguments reads the hosts a trust command acts on, accepting URLs for them.
func hostArguments(current *urfave.Command) ([]string, error) {
	values := current.Args().Slice()
	if len(values) == 0 {
		return nil, fault.New("invalid_arguments", "Specify at least one host, or a URL naming one.")
	}
	hosts := make([]string, 0, len(values))
	for _, value := range values {
		host, err := trust.ParseHost(value)
		if err != nil {
			return nil, fault.Wrap("invalid_arguments", "The host is invalid.", err)
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// without returns the asked-for hosts a change did not touch, without repeats.
func without(asked, changed []string) []string {
	rest := []string{}
	seen := map[string]struct{}{}
	for _, host := range asked {
		if _, done := seen[host]; done {
			continue
		}
		seen[host] = struct{}{}
		if !slices.Contains(changed, host) {
			rest = append(rest, host)
		}
	}
	slices.Sort(rest)
	return rest
}

func trustWriteError(err error) error {
	return fault.Wrap("trust_write_failed", "DAC could not write the trusted-hosts file.", err)
}

// trustListText formats the trusted hosts as one row each.
// The host leads because it is what somebody is looking for, and the time it was
// last used follows because that is what says whether it is still worth keeping.
func trustListText(palette style.Palette, result application.TrustListResult) string {
	if len(result.Hosts) == 0 {
		return "No trusted hosts. Run dac trust add <host>."
	}
	width := 0
	for _, host := range result.Hosts {
		width = max(width, len(host.Host))
	}
	var text strings.Builder
	for _, host := range result.Hosts {
		_, _ = fmt.Fprintf(&text, "%s  %s\n",
			palette.Name(fmt.Sprintf("%-*s", width, host.Host)),
			palette.Detail(usedText(host.LastUsed)))
	}
	_, _ = fmt.Fprintf(&text, "%s.", palette.Strong(plural(len(result.Hosts), "trusted host")))
	return text.String()
}

// usedText writes when a host was last downloaded from.
// A host nothing has used yet says so rather than showing the zero time, which
// would read as a date in the year one.
func usedText(value time.Time) string {
	if value.IsZero() {
		return "never used"
	}
	return value.UTC().Format(time.RFC3339)
}

// trustAddText summarizes one trust addition.
func trustAddText(palette style.Palette, result application.TrustAddResult) string {
	var text strings.Builder
	if len(result.Added) > 0 {
		_, _ = fmt.Fprintf(&text, "Trusted %s.", palette.Name(strings.Join(result.Added, ", ")))
	}
	if len(result.Present) > 0 {
		if text.Len() > 0 {
			text.WriteByte(' ')
		}
		_, _ = fmt.Fprintf(&text, "%s already trusted.", palette.Name(strings.Join(result.Present, ", ")))
	}
	return text.String()
}

// trustRemoveText summarizes one trust removal.
func trustRemoveText(palette style.Palette, result application.TrustRemoveResult) string {
	var text strings.Builder
	if len(result.Removed) > 0 {
		_, _ = fmt.Fprintf(&text, "No longer trusting %s.", palette.Name(strings.Join(result.Removed, ", ")))
	}
	if len(result.Absent) > 0 {
		if text.Len() > 0 {
			text.WriteByte(' ')
		}
		_, _ = fmt.Fprintf(&text, "%s.", palette.Warn(strings.Join(result.Absent, ", ")+" was not trusted"))
	}
	return text.String()
}

// trustGCText summarizes one trust collection.
func trustGCText(palette style.Palette, result application.TrustGCResult) string {
	verb := "No longer trusting"
	if result.DryRun {
		verb = "Would stop trusting"
	}
	if result.HostCount == 0 {
		return "No trusted host is unused."
	}
	return fmt.Sprintf("%s %s: %s.", verb,
		palette.Strong(plural(result.HostCount, "host")), palette.Name(strings.Join(result.Hosts, ", ")))
}
