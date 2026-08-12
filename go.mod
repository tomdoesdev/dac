module github.com/tomdoesdev/dac

go 1.26.5

require (
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/tomdoesdev/kit v0.0.0
	golang.org/x/text v0.33.0
)

require github.com/spf13/pflag v1.0.10 // indirect

// kit is a sibling workspace module and is intentionally not fetched from a
// module proxy. A standalone dac distribution is outside this MVP.
replace github.com/tomdoesdev/kit => ./kit
