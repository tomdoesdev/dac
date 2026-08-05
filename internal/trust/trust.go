// Package trust records the hosts DAC is willing to download from.
// A host DAC has not been told to trust is a host DAC refuses, so that a manifest
// edit cannot silently move a download to somewhere nobody chose.
package trust

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the trusted-hosts file version DAC reads and writes.
const SchemaVersion = 1

// The trusted-hosts file name and the directory it sits in under the XDG data location.
const (
	DirName  = "dac"
	FileName = "trusted-hosts.json"
)

// fileMode is the permission the trusted-hosts file carries.
const fileMode = 0o644

// Entry records when a host was trusted and when DAC last downloaded from it.
type Entry struct {
	AddedAt time.Time `json:"addedAt"`
	// LastUsed is zero until a download reaches this host, which is what collection reads.
	LastUsed time.Time `json:"lastUsed"`
}

// List is the trusted-hosts file as one value.
// Hosts is keyed by host so the file cannot name one host twice, and so a
// membership test is the map lookup it should be.
type List struct {
	SchemaVersion int              `json:"schemaVersion"`
	Hosts         map[string]Entry `json:"hosts"`
}

// Empty returns a list that trusts nothing.
func Empty() List { return List{SchemaVersion: SchemaVersion, Hosts: map[string]Entry{}} }

// Normalize rewrites each key into the form DAC matches on.
// A file naming one host two ways is an error rather than a silent merge, because
// the two entries carry different timestamps and DAC cannot choose between them.
func (list List) Normalize() error {
	for host, entry := range list.Hosts {
		normalized := Normalize(host)
		if normalized == host {
			continue
		}
		if _, exists := list.Hosts[normalized]; exists {
			return fmt.Errorf("trusted hosts name %q and %q, which are the same host", host, normalized)
		}
		delete(list.Hosts, host)
		list.Hosts[normalized] = entry
	}
	return nil
}

// Validate checks the schema version and each host name.
func (list List) Validate() error {
	if list.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported trusted-hosts schema version %d, DAC understands %d",
			list.SchemaVersion, SchemaVersion)
	}
	if list.Hosts == nil {
		return errors.New("trusted hosts must be an object")
	}
	for host := range list.Hosts {
		if host == "" {
			return errors.New("a trusted host is empty")
		}
		if Normalize(host) != host {
			return fmt.Errorf("trusted host %q is not a bare lowercase host name", host)
		}
	}
	return nil
}

// Trusted reports whether DAC may download from a host.
func (list List) Trusted(host string) bool {
	_, found := list.Hosts[Normalize(host)]
	return found
}

// Sorted returns the trusted hosts in name order.
func (list List) Sorted() []string {
	hosts := slices.Collect(maps.Keys(list.Hosts))
	slices.Sort(hosts)
	return hosts
}

// Add trusts each host and reports the ones the list did not already name.
func (list List) Add(hosts []string, now time.Time) (List, []string) {
	updated := list.clone()
	added := []string{}
	for _, host := range hosts {
		name := Normalize(host)
		if _, exists := updated.Hosts[name]; exists {
			continue
		}
		updated.Hosts[name] = Entry{AddedAt: now}
		added = append(added, name)
	}
	slices.Sort(added)
	return updated, added
}

// Remove withdraws trust from each host and reports the ones the list named.
func (list List) Remove(hosts []string) (List, []string) {
	updated := list.clone()
	removed := []string{}
	for _, host := range hosts {
		name := Normalize(host)
		if _, exists := updated.Hosts[name]; !exists {
			continue
		}
		delete(updated.Hosts, name)
		removed = append(removed, name)
	}
	slices.Sort(removed)
	return updated, removed
}

// Touch records a download from each host the list already names.
// A host the list does not name is ignored, so a run that was already in flight
// does not put back a host somebody removed while it ran.
func (list List) Touch(hosts []string, now time.Time) List {
	updated := list.clone()
	for _, host := range hosts {
		name := Normalize(host)
		entry, exists := updated.Hosts[name]
		if !exists {
			continue
		}
		entry.LastUsed = now
		updated.Hosts[name] = entry
	}
	return updated
}

// Collect drops each host last used before the cutoff and reports them.
// An entry carrying no times at all is kept: a hand-written file states trust
// without stating when, and nothing can be proven stale from nothing.
func (list List) Collect(cutoff time.Time) (List, []string) {
	updated := list.clone()
	collected := []string{}
	for host, entry := range updated.Hosts {
		used := entry.LastUsed
		if used.IsZero() {
			used = entry.AddedAt
		}
		if used.IsZero() || !used.Before(cutoff) {
			continue
		}
		delete(updated.Hosts, host)
		collected = append(collected, host)
	}
	slices.Sort(collected)
	return updated, collected
}

// clone copies the list so every operation returns a new value.
func (list List) clone() List {
	hosts := maps.Clone(list.Hosts)
	if hosts == nil {
		hosts = map[string]Entry{}
	}
	return List{SchemaVersion: SchemaVersion, Hosts: hosts}
}

// ParseHost reduces a host or a URL to the name DAC matches on.
// A URL is accepted because the value somebody has to hand is the one in the
// manifest, and retyping its host is a chance to trust the wrong one.
func ParseHost(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("host is empty")
	}
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("URL %q is invalid: %w", value, err)
		}
		host := Normalize(parsed.Hostname())
		if host == "" {
			return "", fmt.Errorf("URL %q names no host", value)
		}
		return host, nil
	}
	host := Normalize(trimmed)
	if host == "" || strings.ContainsAny(host, "/?#@ \t") {
		return "", fmt.Errorf("host %q is invalid", value)
	}
	return host, nil
}

// Normalize is the matching rule: lower case, no port, no brackets, no root dot.
// Matching is exact after this, so a Unicode host name is trusted in the punycode
// form a URL carries rather than the form somebody reads.
func Normalize(host string) string {
	value := strings.ToLower(strings.TrimSpace(host))
	// A port is part of an address rather than of who is on the other end, and one
	// origin serving two ports is still one origin to trust.
	if stripped, _, err := net.SplitHostPort(value); err == nil {
		value = stripped
	}
	value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	return strings.TrimSuffix(value, ".")
}
