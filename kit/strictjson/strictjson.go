// Package strictjson decodes JSON that must be unambiguous.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
)

var (
	// ErrMultipleValues marks input containing data after its first JSON value.
	ErrMultipleValues = errors.New("JSON contains more than one value")
	// ErrInvalidDelimiter marks a closing delimiter without its matching opener.
	ErrInvalidDelimiter = errors.New("JSON has an invalid delimiter")
	// ErrMissingObjectValue marks an object key without a following value.
	ErrMissingObjectValue = errors.New("JSON object is missing a value")
	// ErrNonStringObjectKey marks an object token that cannot be a JSON key.
	ErrNonStringObjectKey = errors.New("JSON object key is not a string")
	// ErrDuplicateObjectKey marks a repeated key in the same JSON object.
	ErrDuplicateObjectKey = errors.New("duplicate JSON object key")
	// ErrFieldNameCaseMismatch marks a struct field matched only by folded case.
	ErrFieldNameCaseMismatch = errors.New("JSON object key does not match a field name exactly")
)

// Unmarshal decodes exactly one JSON value into value. It rejects duplicate
// object keys, unknown struct members, and case-insensitive field matches
// unless an option explicitly permits those compatibility behaviours.
func Unmarshal(data []byte, value any, options ...Option) error {
	settings := newSettings(options)
	if err := scan(data, decodeType(value), settings); err != nil {
		return err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	if !settings.allowUnknownMembers {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrMultipleValues
		}
		return err
	}
	return nil
}

// ReadFile reads path and decodes it with Unmarshal.
func ReadFile(path string, value any, options ...Option) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return Unmarshal(data, value, options...)
}

// frame records the parser state for one object or array without recursion.
// An explicit stack avoids turning deeply nested, untrusted JSON into Go calls.
type frame struct {
	delimiter    json.Delim
	expectingKey bool
	keys         map[string]struct{}
	fields       structFields
	elementType  reflect.Type
	nextType     reflect.Type
}

// scan walks tokens once to reject duplicate keys and optionally detect field
// names that encoding/json would otherwise match only by folding their case.
func scan(data []byte, root reflect.Type, settings settings) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var stack []frame
	rootDone := false

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if len(stack) != 0 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}

		if delimiter, ok := token.(json.Delim); ok && (delimiter == '}' || delimiter == ']') {
			if len(stack) == 0 || stack[len(stack)-1].delimiter != matchingOpen(delimiter) {
				return ErrInvalidDelimiter
			}
			if delimiter == '}' && !stack[len(stack)-1].expectingKey {
				return ErrMissingObjectValue
			}
			stack = stack[:len(stack)-1]
			rootDone = completeValue(stack, rootDone)
			continue
		}

		if len(stack) > 0 && stack[len(stack)-1].delimiter == '{' && stack[len(stack)-1].expectingKey {
			key, ok := token.(string)
			if !ok {
				return ErrNonStringObjectKey
			}
			current := &stack[len(stack)-1]
			if _, exists := current.keys[key]; exists {
				return positionError(data, decoder.InputOffset(), fmt.Errorf("%w %q", ErrDuplicateObjectKey, key))
			}
			current.keys[key] = struct{}{}
			if current.elementType != nil {
				// Maps use the same value type for every object member.
				current.nextType = current.elementType
			} else if !settings.matchCaseInsensitiveNames {
				if type_, ok := current.fields.byName[key]; ok {
					current.nextType = type_
				} else if foldedField(current.fields.names, key) {
					return positionError(data, decoder.InputOffset(), fmt.Errorf("%w: %q", ErrFieldNameCaseMismatch, key))
				}
			} else {
				current.nextType = matchingFieldType(current.fields, key)
			}
			current.expectingKey = false
			continue
		}

		if rootDone && len(stack) == 0 {
			return ErrMultipleValues
		}
		type_ := root
		if len(stack) > 0 {
			current := &stack[len(stack)-1]
			if current.delimiter == '{' {
				type_ = current.nextType
			} else {
				type_ = current.elementType
			}
		}

		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				stack = append(stack, newObjectFrame(type_))
				continue
			case '[':
				stack = append(stack, newArrayFrame(type_))
				continue
			default:
				return ErrInvalidDelimiter
			}
		}
		rootDone = completeValue(stack, rootDone)
	}
}

func newObjectFrame(type_ reflect.Type) frame {
	result := frame{delimiter: '{', expectingKey: true, keys: make(map[string]struct{})}
	type_ = inspectType(type_)
	if type_ == nil {
		return result
	}
	switch type_.Kind() {
	case reflect.Struct:
		result.fields = fieldsFor(type_)
	case reflect.Map:
		result.elementType = type_.Elem()
	}
	return result
}

func newArrayFrame(type_ reflect.Type) frame {
	result := frame{delimiter: '['}
	type_ = inspectType(type_)
	if type_ != nil && (type_.Kind() == reflect.Array || type_.Kind() == reflect.Slice) {
		result.elementType = type_.Elem()
	}
	return result
}

// inspectType keeps the scanner out of values whose JSON representation is
// owned by a custom unmarshaler, then follows pointers to their concrete kind.
func inspectType(type_ reflect.Type) reflect.Type {
	if hasCustomUnmarshaler(type_) {
		return nil
	}
	return indirectType(type_)
}

func decodeType(value any) reflect.Type {
	type_ := reflect.TypeOf(value)
	if type_ != nil && type_.Kind() == reflect.Pointer {
		return type_.Elem()
	}
	return nil
}

func completeValue(stack []frame, rootDone bool) bool {
	if len(stack) == 0 {
		return true
	}
	parent := &stack[len(stack)-1]
	if parent.delimiter == '{' {
		parent.expectingKey = true
		parent.nextType = nil
	}
	return rootDone
}

func matchingOpen(close json.Delim) json.Delim {
	if close == '}' {
		return '{'
	}
	return '['
}

func foldedField(names []string, key string) bool {
	for _, name := range names {
		if strings.EqualFold(name, key) {
			return true
		}
	}
	return false
}

func matchingFieldType(fields structFields, key string) reflect.Type {
	if type_, ok := fields.byName[key]; ok {
		return type_
	}
	for name, type_ := range fields.byName {
		if strings.EqualFold(name, key) {
			return type_
		}
	}
	return nil
}

// positionError attaches a source location without hiding the parser sentinel.
func positionError(data []byte, offset int64, err error) error {
	line, column := lineColumn(data, offset)
	return fmt.Errorf("%w at %d:%d", err, line, column)
}

func lineColumn(data []byte, offset int64) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	line, column := 1, 1
	for _, character := range data[:offset] {
		if character == '\n' {
			line, column = line+1, 1
		} else {
			column++
		}
	}
	return line, column
}
