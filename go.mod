module github.com/tomdoesdev/dac

go 1.26.5

require (
	github.com/mattn/go-isatty v0.0.20
	github.com/mattn/go-runewidth v0.0.27
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/tomdoesdev/kit v0.0.0
	github.com/vbauerster/mpb/v8 v8.14.0
	golang.org/x/text v0.33.0
)

require (
	github.com/VividCortex/ewma v1.2.0 // indirect
	github.com/acarl005/stripansi v0.0.0-20180116102854-5a71ef0e047d // indirect
	github.com/aymanbagabas/go-osc52/v2 v2.0.1 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.2.0 // indirect
	github.com/muesli/termenv v0.16.0 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/vbauerster/cupwriter v0.0.4 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// kit is a sibling workspace module and is intentionally not fetched from a
// module proxy. A standalone dac distribution is outside this MVP.
replace github.com/tomdoesdev/kit => ./kit
