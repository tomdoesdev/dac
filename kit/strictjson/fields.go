package strictjson

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// structFields is the small part of encoding/json's field resolution that the
// case check needs. The standard decoder remains the authority for rejecting
// unknown members; this only identifies a known field whose spelling differs.
type structFields struct {
	byName map[string]reflect.Type
	names  []string
}

type fieldCandidate struct {
	name   string
	type_  reflect.Type
	index  []int
	tagged bool
}

var structFieldsCache sync.Map // map[reflect.Type]structFields

func fieldsFor(type_ reflect.Type) structFields {
	if type_ == nil || type_.Kind() != reflect.Struct {
		return structFields{}
	}
	if cached, ok := structFieldsCache.Load(type_); ok {
		return cached.(structFields)
	}

	// Competing first calls may each compute these immutable fields. Keeping the
	// work lock-free makes decoding a configuration file never wait on another.
	resolved := resolveFields(type_)
	actual, _ := structFieldsCache.LoadOrStore(type_, resolved)
	return actual.(structFields)
}

// resolveFields follows encoding/json's embedded-field precedence closely
// enough to safely select the type cursor. Ambiguous fields are omitted, which
// leaves the standard decoder to reject them as unknown.
func resolveFields(root reflect.Type) structFields {
	var candidates []fieldCandidate
	collectFields(root, nil, map[reflect.Type]bool{}, &candidates)
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].name != candidates[right].name {
			return candidates[left].name < candidates[right].name
		}
		if len(candidates[left].index) != len(candidates[right].index) {
			return len(candidates[left].index) < len(candidates[right].index)
		}
		if candidates[left].tagged != candidates[right].tagged {
			return candidates[left].tagged
		}
		return lessIndex(candidates[left].index, candidates[right].index)
	})

	result := structFields{byName: make(map[string]reflect.Type)}
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].name == candidates[start].name {
			end++
		}
		if field, ok := dominantField(candidates[start:end]); ok {
			result.byName[field.name] = field.type_
			result.names = append(result.names, field.name)
		}
		start = end
	}
	return result
}

// collectFields expands untagged embedded structs. A type on the active path
// is not revisited, which makes recursive embeddings finite.
func collectFields(type_ reflect.Type, prefix []int, visiting map[reflect.Type]bool, candidates *[]fieldCandidate) {
	if type_ == nil || type_.Kind() != reflect.Struct || visiting[type_] {
		return
	}
	visiting[type_] = true
	defer delete(visiting, type_)

	for index := 0; index < type_.NumField(); index++ {
		field := type_.Field(index)
		if field.PkgPath != "" && !field.Anonymous {
			continue
		}
		tag := field.Tag.Get("json")
		name, tagged := parseJSONTag(tag)
		if tag == "-" {
			continue
		}
		path := append(append([]int(nil), prefix...), index)
		base := indirectType(field.Type)
		if field.Anonymous && !tagged && base != nil && base.Kind() == reflect.Struct {
			collectFields(base, path, visiting, candidates)
			continue
		}
		if field.PkgPath != "" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		*candidates = append(*candidates, fieldCandidate{name: name, type_: field.Type, index: path, tagged: tagged})
	}
}

// dominantField selects the one field encoding/json would use. Equal-depth
// contenders without a tagged winner are ambiguous and intentionally omitted.
func dominantField(fields []fieldCandidate) (fieldCandidate, bool) {
	first := fields[0]
	if len(fields) == 1 {
		return first, true
	}
	second := fields[1]
	if len(first.index) == len(second.index) && first.tagged == second.tagged {
		return fieldCandidate{}, false
	}
	return first, true
}

func lessIndex(left, right []int) bool {
	for index := range left {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return false
}

func parseJSONTag(tag string) (string, bool) {
	name, _, _ := strings.Cut(tag, ",")
	if !validTag(name) {
		return "", false
	}
	return name, name != ""
}

// validTag matches encoding/json's deliberately broad definition of a field
// name, including Unicode letters and digits but excluding JSON punctuation.
func validTag(tag string) bool {
	if tag == "" {
		return true
	}
	for _, character := range tag {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:<=>?@[]^_{|}~ ", character):
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character >= 0x80:
		default:
			return false
		}
	}
	return true
}

func indirectType(type_ reflect.Type) reflect.Type {
	for type_ != nil && type_.Kind() == reflect.Pointer {
		type_ = type_.Elem()
	}
	return type_
}

// hasCustomUnmarshaler stops type-directed inspection at user-owned decoding.
// Such a type controls what its JSON object means, not its Go fields.
func hasCustomUnmarshaler(type_ reflect.Type) bool {
	if type_ == nil {
		return false
	}
	unmarshaler := reflect.TypeFor[json.Unmarshaler]()
	return type_.Implements(unmarshaler) || reflect.PointerTo(indirectType(type_)).Implements(unmarshaler)
}
