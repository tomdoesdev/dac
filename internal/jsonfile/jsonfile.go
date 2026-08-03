// Package jsonfile reads strict JSON files and writes atomic JSON files.
package jsonfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// TempPrefix names the temporary files WriteAtomic creates. It is exported so
// that a caller sweeping a directory DAC writes into can recognize the ones an
// interrupted write left behind.
const TempPrefix = ".dac-"

// WriteAtomic writes data through a synced temporary file and one rename.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, TempPrefix+"*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// maxDepth bounds how deeply the duplicate-key scan will nest.
//
// The scan walks the document with one stack frame per level, so without a
// bound a document of nothing but open brackets exhausts the goroutine stack --
// which the runtime reports as a fatal error rather than a panic, so nothing
// above can turn it into a failed command. A cache bundle's index arrives from
// wherever the bundle came from, and the index size cap alone leaves room for
// millions of levels. Project files nest three deep, so this is far past
// anything a real document reaches.
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
