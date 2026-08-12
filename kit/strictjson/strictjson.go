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
			return errors.New("JSON contains more than one value")
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
				return errors.New("JSON has an invalid delimiter")
			}
			if delimiter == '}' && !stack[len(stack)-1].expectingKey {
				return errors.New("JSON object is missing a value")
			}
			stack = stack[:len(stack)-1]
			rootDone = completeValue(stack, rootDone)
			continue
		}

		if len(stack) > 0 && stack[len(stack)-1].delimiter == '{' && stack[len(stack)-1].expectingKey {
			key, ok := token.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			current := &stack[len(stack)-1]
			if _, exists := current.keys[key]; exists {
				return positionError(data, decoder.InputOffset(), "duplicate JSON object key %q", key)
			}
			current.keys[key] = struct{}{}
			if current.elementType != nil {
				// Maps use the same value type for every object member.
				current.nextType = current.elementType
			} else if !settings.matchCaseInsensitiveNames {
				if type_, ok := current.fields.byName[key]; ok {
					current.nextType = type_
				} else if foldedField(current.fields.names, key) {
					return positionError(data, decoder.InputOffset(), "JSON object key %q does not match a field name exactly", key)
				}
			} else {
				current.nextType = matchingFieldType(current.fields, key)
			}
			current.expectingKey = false
			continue
		}

		if rootDone && len(stack) == 0 {
			return errors.New("JSON contains more than one value")
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
				return errors.New("JSON has an invalid delimiter")
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

func positionError(data []byte, offset int64, format string, arguments ...any) error {
	line, column := lineColumn(data, offset)
	return fmt.Errorf(format+" at %d:%d", append(arguments, line, column)...)
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
