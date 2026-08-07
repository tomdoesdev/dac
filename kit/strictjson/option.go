package strictjson

// Option changes how Unmarshal and ReadFile decode JSON.
type Option func(*settings)

// settings holds the decoder behaviours options can relax.
type settings struct {
	allowUnknownMembers       bool
	matchCaseInsensitiveNames bool
}

func newSettings(options []Option) settings {
	var current settings
	for _, option := range options {
		option(&current)
	}
	return current
}

// AllowUnknownMembers accepts object members that have no destination in a
// struct. It is useful for API responses that may add fields over time.
func AllowUnknownMembers() Option {
	return func(current *settings) { current.allowUnknownMembers = true }
}

// MatchCaseInsensitiveNames accepts the case-insensitive struct-field matching
// that encoding/json uses by default. It helps migrate existing inputs.
func MatchCaseInsensitiveNames() Option {
	return func(current *settings) { current.matchCaseInsensitiveNames = true }
}
