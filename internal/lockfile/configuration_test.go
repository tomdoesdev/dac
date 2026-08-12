package lockfile

import (
	"testing"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/manifest"
)

func TestConfigurationDigestCoversDesiredSourcePolicy(t *testing.T) {
	base := manifest.ResolvedAsset{
		Name: "artifact",
		Asset: manifest.Asset{
			URL: "https://example.com/{{.VERSION}}", File: "artifact-{{.VERSION}}.bin",
			Pin:       "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Variables: map[string]string{"VERSION": "one", "UNUSED": "value"},
			Headers:   map[string]string{"Authorization": "env:TOKEN"},
		},
		ResolvedURL: "https://example.com/one", ResolvedFile: "artifact-one.bin",
		Transfer: asset.DefaultTransferPolicy(),
	}
	want := ConfigurationDigest(base)
	changes := map[string]func(*manifest.ResolvedAsset){
		"URL template":      func(value *manifest.ResolvedAsset) { value.URL += "?mirror=one" },
		"file template":     func(value *manifest.ResolvedAsset) { value.File = "other-{{.VERSION}}.bin" },
		"unused variable":   func(value *manifest.ResolvedAsset) { value.Variables["UNUSED"] = "changed" },
		"pin removal":       func(value *manifest.ResolvedAsset) { value.Pin = "" },
		"configured header": func(value *manifest.ResolvedAsset) { value.Headers["Authorization"] = "env:OTHER_TOKEN" },
		"maximum size":      func(value *manifest.ResolvedAsset) { value.Transfer.MaxSize++ },
		"idle timeout":      func(value *manifest.ResolvedAsset) { value.Transfer.IdleTimeout++ },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.Variables = cloneStrings(base.Variables)
			candidate.Headers = cloneStrings(base.Headers)
			change(&candidate)
			if got := ConfigurationDigest(candidate); got == want {
				t.Fatalf("ConfigurationDigest did not change")
			}
		})
	}

	caseOnly := base
	caseOnly.Headers = map[string]string{"authorization": "env:TOKEN"}
	if got := ConfigurationDigest(caseOnly); got != want {
		t.Fatalf("header casing changed digest: %s != %s", got, want)
	}
}

func cloneStrings(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
