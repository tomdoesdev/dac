// Package coord defines DAC asset coordinates.
//
// A coordinate names one asset completely: <namespace>/<name>@<version>. DAC
// keys the manifest and the lock file by it, so a version is part of what an
// asset is rather than a field describing it. That is what makes a version
// mean something. Two versions of one asset are two entries, a project can
// hold both, and editing a version cannot quietly redefine the asset that was
// already there.
//
// The namespace exists so that two projects, or two parts of one project, can
// each have a "database" without either one being the database.
package coord

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// maxPart bounds each part of a coordinate. A coordinate is a map key, a
// progress label, and a command argument, and none of those want an unbounded
// string.
const maxPart = 64

// The three parts answer to different things, so they take different
// characters. A namespace and a name are labels this project chooses, and they
// are lowercase because two spellings of one asset would be two assets: a case
// difference is the spelling mistake nobody sees. A version is copied from
// whatever the publisher calls a release, so it keeps uppercase and the plus
// sign that build metadata uses.
const (
	labelSeparators   = "._-"
	versionSeparators = "._-+"
	labelCharacters   = "lowercase letters, digits, and . _ -"
	versionCharacters = "letters, digits, and . _ - +"
)

// Group is the pair a version belongs to. Every coordinate sharing a namespace
// and a name names the same asset at a different version, and that is the set
// the version rules apply within.
type Group struct {
	Namespace string
	Name      string
}

func (group Group) String() string {
	return group.Namespace + "/" + group.Name
}

// Validate checks the two parts of a group.
func (group Group) Validate() error {
	if err := checkPart("namespace", group.Namespace, false, labelSeparators, labelCharacters); err != nil {
		return err
	}
	return checkPart("name", group.Name, false, labelSeparators, labelCharacters)
}

// Coordinate is the complete identity of one asset.
type Coordinate struct {
	Namespace string
	Name      string
	Version   string
}

// Group returns the asset this coordinate is a version of.
func (value Coordinate) Group() Group {
	return Group{Namespace: value.Namespace, Name: value.Name}
}

func (value Coordinate) String() string {
	return value.Namespace + "/" + value.Name + "@" + value.Version
}

// Validate checks all three parts.
func (value Coordinate) Validate() error {
	if err := value.Group().Validate(); err != nil {
		return err
	}
	return checkPart("version", value.Version, true, versionSeparators, versionCharacters)
}

// Parse reads one complete coordinate.
func Parse(text string) (Coordinate, error) {
	if !separated(text, "/", "@") {
		return Coordinate{}, fmt.Errorf("coordinate %q must be <namespace>/<name>@<version>", text)
	}
	namespace, rest, _ := strings.Cut(text, "/")
	name, version, _ := strings.Cut(rest, "@")
	value := Coordinate{Namespace: namespace, Name: name, Version: version}
	if err := value.Validate(); err != nil {
		return Coordinate{}, fmt.Errorf("coordinate %q: %w", text, err)
	}
	return value, nil
}

// MustParse parses a coordinate that the caller knows is valid. It is for
// constants and tests; anything reading a coordinate from a file or an argument
// uses Parse and reports the error.
func MustParse(text string) Coordinate {
	value, err := Parse(text)
	if err != nil {
		panic(err)
	}
	return value
}

// ParseGroup reads one namespace and name with no version.
func ParseGroup(text string) (Group, error) {
	if strings.Count(text, "/") != 1 || strings.Contains(text, "@") {
		return Group{}, fmt.Errorf("asset %q must be <namespace>/<name>", text)
	}
	namespace, name, _ := strings.Cut(text, "/")
	group := Group{Namespace: namespace, Name: name}
	if err := group.Validate(); err != nil {
		return Group{}, fmt.Errorf("asset %q: %w", text, err)
	}
	return group, nil
}

// MarshalText writes the coordinate that keys a manifest or lock entry.
//
// It validates first. A Coordinate built in memory rather than parsed could
// otherwise put a key into a project file that DAC would refuse to read back,
// and a file DAC cannot read is a worse failure than a write that refused.
func (value Coordinate) MarshalText() ([]byte, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return []byte(value.String()), nil
}

// UnmarshalText reads one coordinate key. Every manifest and lock key passes
// through here, so an invalid coordinate fails at the read that found it rather
// than at whichever command first tried to use it.
func (value *Coordinate) UnmarshalText(data []byte) error {
	parsed, err := Parse(string(data))
	if err != nil {
		return err
	}
	*value = parsed
	return nil
}

// Compare orders coordinates by namespace, then name, then version.
//
// All three comparisons are lexicographic, including the version. DAC does not
// order versions: it has no idea whether "10" follows "9" or whether either is
// a number. This exists to make output and file writes deterministic, and it is
// deliberately not something a caller should read meaning into.
func Compare(first, second Coordinate) int {
	if result := strings.Compare(first.Namespace, second.Namespace); result != 0 {
		return result
	}
	if result := strings.Compare(first.Name, second.Name); result != 0 {
		return result
	}
	return strings.Compare(first.Version, second.Version)
}

// Sorted returns the keys of a coordinate-keyed map in a stable order.
func Sorted[V any](values map[Coordinate]V) []Coordinate {
	return slices.SortedFunc(maps.Keys(values), Compare)
}

// InGroup returns the coordinates of one asset, sorted. It answers the question
// every "which versions do I have" message asks, from the sibling list an add
// reports to the versions an unknown coordinate suggests instead.
func InGroup[V any](values map[Coordinate]V, group Group) []Coordinate {
	found := make([]Coordinate, 0, len(values))
	for value := range values {
		if value.Group() == group {
			found = append(found, value)
		}
	}
	slices.SortFunc(found, Compare)
	return found
}

// Strings renders coordinates for a message or a JSON list.
func Strings(values []Coordinate) []string {
	text := make([]string, 0, len(values))
	for _, value := range values {
		text = append(text, value.String())
	}
	return text
}

// Versions renders only the version part of each coordinate, which is what a
// message that has already named the asset should list.
func Versions(values []Coordinate) []string {
	versions := make([]string, 0, len(values))
	for _, value := range values {
		versions = append(versions, value.Version)
	}
	return versions
}

// separated reports whether text holds exactly one of each separator, in the
// order given. It is what keeps "a@b/c" from parsing as a namespace containing
// an at sign.
func separated(text string, separators ...string) bool {
	position := -1
	for _, separator := range separators {
		if strings.Count(text, separator) != 1 {
			return false
		}
		found := strings.Index(text, separator)
		if found < position {
			return false
		}
		position = found
	}
	return true
}

// checkPart checks one part of a coordinate against its character set.
//
// A part starts and ends alphanumeric. That rules out the leading dash that
// reads as a flag wherever a coordinate is a command argument, the trailing dot
// that reads as a typo, and "." and ".." outright.
func checkPart(kind, value string, allowUpper bool, separators, characters string) error {
	switch {
	case value == "":
		return fmt.Errorf("the %s is empty", kind)
	case len(value) > maxPart:
		return fmt.Errorf("the %s %q is longer than %d characters", kind, value, maxPart)
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if alphanumeric(character, allowUpper) {
			continue
		}
		if !strings.ContainsRune(separators, rune(character)) {
			return fmt.Errorf("the %s %q contains %q; a %s uses %s", kind, value, string(character), kind, characters)
		}
		if index == 0 || index == len(value)-1 {
			return fmt.Errorf("the %s %q starts or ends with %q", kind, value, string(character))
		}
	}
	return nil
}

func alphanumeric(character byte, allowUpper bool) bool {
	switch {
	case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
		return true
	case allowUpper && character >= 'A' && character <= 'Z':
		return true
	}
	return false
}
