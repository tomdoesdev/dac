// Package jsonfile reads strict JSON files and writes atomic JSON files.
package jsonfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tomdoesdev/dac/internal/fs/atomic"
	"github.com/tomdoesdev/dac/internal/fs/flock"
)

// ReadStrict reads one JSON value. It rejects unknown fields and duplicate keys.
func ReadStrict(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return DecodeStrict(data, value)
}

// DecodeStrict decodes one JSON value. It rejects unknown fields and duplicate keys.
func DecodeStrict(data []byte, value any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
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

// WriteAtomic locks path, writes data through a synced temporary file, and renames it once.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	return flock.Hold(context.Background(), path+".lock", func(context.Context) error {
		return atomic.WriteFile(path, data, mode)
	})
}

// maxDepth bounds how deeply the duplicate-key scan will nest.
// The depth bound prevents open brackets from exhausting the goroutine stack.
const maxDepth = 128

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := visitValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains more than one value")
		}
		return err
	}
	return nil
}

func visitValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxDepth {
		return fmt.Errorf("JSON nests more than %d levels deep", maxDepth)
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			keys[key] = struct{}{}
			if err := visitValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := visitValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("JSON has an invalid delimiter")
	}
	_, err = decoder.Token()
	return err
}
