// Package credential asks an external helper program for request headers.
//
// A helper keeps credential creation outside DAC. DAC gives the request URL to
// a selected helper and uses the headers that the helper returns.
package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/tomdoesdev/dac/internal/debug"
)

// maxOutput bounds a helper's response so a runaway program cannot exhaust
// memory while DAC waits for it.
const maxOutput = 1 << 20

// waitGrace is how long DAC waits for a killed helper's output pipes to close
// before it abandons them.
//
// Killing the helper is not enough on its own. A helper is usually a shell
// script, and a script that runs "aws", "vault", or "sleep" hands that child the
// same stdout and stderr pipes. Killing the script leaves the grandchild holding
// the write end, and the read that collects the helper's output blocks until the
// grandchild exits -- which is the hang the timeout exists to prevent, arriving
// through the back door. WaitDelay closes the pipes instead of waiting for
// whoever still holds them.
const waitGrace = time.Second

// Resolver runs credential helpers for asset requests.
type Resolver struct {
	helpers []helper
	timeout time.Duration
	// Logger traces which helper answered for which host. It never carries what
	// a helper returned: see Headers.
	Logger *slog.Logger
}

type helper struct {
	host    string
	command string
}

type request struct {
	URI string `json:"uri"`
}

type response struct {
	Headers map[string][]string `json:"headers"`
}

// New builds a resolver from helper specifications.
//
// A specification is either "<command>", which applies to every host, or
// "<host>=<command>", which applies to one. A host-specific helper wins over a
// general one, so a project can point one registry at a dedicated helper
// without giving that helper every other request.
func New(specifications []string, timeout time.Duration) (*Resolver, error) {
	if len(specifications) == 0 {
		return nil, nil
	}
	if timeout <= 0 {
		return nil, errors.New("the credential helper timeout must be positive")
	}
	resolver := &Resolver{timeout: timeout}
	for _, specification := range specifications {
		value := strings.TrimSpace(specification)
		if value == "" {
			return nil, errors.New("a credential helper must not be empty")
		}
		host, command := "", value
		if key, rest, found := strings.Cut(value, "="); found {
			host, command = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(rest)
		}
		if command == "" {
			return nil, fmt.Errorf("credential helper %q has no command", specification)
		}
		resolver.helpers = append(resolver.helpers, helper{host: host, command: command})
	}
	return resolver, nil
}

// Headers returns the headers a helper supplies for one request URL. It reports
// no headers when no helper covers the host.
func (resolver *Resolver) Headers(ctx context.Context, rawURL string) (http.Header, error) {
	if resolver == nil || len(resolver.helpers) == 0 {
		return nil, nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(parsed.Hostname())
	selected, found := resolver.selectHelper(host)
	trace := debug.Or(resolver.Logger)
	if !found {
		trace.Debug("no credential helper for host", "host", host)
		return nil, nil
	}
	trace.Debug("running credential helper", "host", host, "command", selected.command)
	payload, err := json.Marshal(request{URI: rawURL})
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, resolver.timeout)
	defer cancel()

	command := exec.CommandContext(runCtx, selected.command, "get")
	command.WaitDelay = waitGrace
	command.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	command.Stdout = &limitedWriter{buffer: &stdout, remaining: maxOutput}
	command.Stderr = &limitedWriter{buffer: &stderr, remaining: 4096}
	if err := command.Run(); err != nil {
		if runCtx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("credential helper %q timed out after %s", selected.command, resolver.timeout)
		}
		// The helper itself finished, but something it started still holds the
		// pipes DAC reads. Naming that is worth a case of its own: the generic
		// wording sends an operator looking at a helper that did nothing wrong.
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil, fmt.Errorf("credential helper %q left a child process holding its output", selected.command)
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("credential helper %q failed: %w: %s", selected.command, err, detail)
		}
		return nil, fmt.Errorf("credential helper %q failed: %w", selected.command, err)
	}
	// The response carries a secret. No part of it belongs in an error message.
	var decoded response
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		return nil, fmt.Errorf("credential helper %q returned invalid JSON", selected.command)
	}
	header := http.Header{}
	for name, values := range decoded.Headers {
		for _, value := range values {
			header.Add(name, value)
		}
	}
	if len(header) == 0 {
		trace.Debug("credential helper returned no headers", "host", host, "command", selected.command)
		return nil, nil
	}
	// The names and nothing else. A trace exists to show which helper answered
	// and what it set, and a helper's answer is the secret itself -- the same
	// reason no part of it reaches an error message either. Sorted so that two
	// runs of one helper read the same.
	names := slices.Sorted(maps.Keys(header))
	trace.Debug("credential helper answered", "host", host, "command", selected.command, "headers", names)
	return header, nil
}

// selectHelper prefers a helper bound to the host over one bound to every host.
func (resolver *Resolver) selectHelper(host string) (helper, bool) {
	var general helper
	var hasGeneral bool
	for _, current := range resolver.helpers {
		if current.host == host {
			return current, true
		}
		if current.host == "" && !hasGeneral {
			general, hasGeneral = current, true
		}
	}
	return general, hasGeneral
}

type limitedWriter struct {
	buffer    *bytes.Buffer
	remaining int
}

func (writer *limitedWriter) Write(data []byte) (int, error) {
	if writer.remaining <= 0 {
		return len(data), nil
	}
	if len(data) > writer.remaining {
		writer.buffer.Write(data[:writer.remaining])
		writer.remaining = 0
		return len(data), nil
	}
	writer.buffer.Write(data)
	writer.remaining -= len(data)
	return len(data), nil
}
