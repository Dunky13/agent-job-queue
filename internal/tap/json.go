package tap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf8"
)

// The backend and remote packages keep their strict-JSON and bounded-output
// helpers unexported; exporting them for one more caller would widen their
// API for no gain, so this package keeps small equivalents.

// checkJSON rejects invalid UTF-8, duplicate object keys, trailing data and
// excessive nesting, which json.Unmarshal alone silently accepts.
func checkJSON(body []byte) error {
	if !utf8.Valid(body) {
		return errors.New("Invalid UTF-8 JSON metadata")
	}
	d := json.NewDecoder(bytes.NewReader(body))
	var walk func(int) error
	walk = func(depth int) error {
		if depth >= 64 {
			return errors.New("JSON nesting exceeds its bound")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		keys := map[string]bool{}
		for d.More() {
			if delim == '{' {
				keyToken, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || keys[key] {
					return errors.New("Invalid or duplicate JSON field")
				}
				keys[key] = true
			}
			if err := walk(depth + 1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return errors.New("Invalid GitHub JSON metadata")
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("Invalid trailing JSON metadata")
	}
	return nil
}

// decode parses a GitHub API response into a typed value. GitHub adds fields
// freely, so unknown fields are tolerated; duplicates and malformed JSON are not.
func decode[T any](body []byte) (T, error) {
	var value T
	if err := checkJSON(body); err != nil {
		return value, err
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return value, errors.New("Unexpected GitHub metadata shape")
	}
	return value, nil
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(value []byte) (int, error) {
	if len(value) > b.limit-b.Len() {
		return 0, errors.New("Output exceeds bound")
	}
	return b.Buffer.Write(value)
}

func boundedFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, errors.New("Required metadata is not a bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errors.New("Required metadata exceeds its size limit")
	}
	return body, nil
}
