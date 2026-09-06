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

func ensureParent(path string) error {
	if path == "" {
		return fmt.Errorf("output path is empty")
	}
	return os.MkdirAll(filepath.Dir(path), 0o755)
}
