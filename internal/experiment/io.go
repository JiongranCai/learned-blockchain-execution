package experiment

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func WriteJSONLines(path string, values []any) error {
	if err := ensureParent(path); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func WriteJSON(path string, value any) error {
	if err := ensureParent(path); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".validation-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func ReadJSON(path string, target any) error {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := decodeStrict(encoded, target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func ensureParent(path string) error {
	if path == "" {
		return fmt.Errorf("output path is empty")
	}
	return os.MkdirAll(filepath.Dir(path), 0o755)
}
