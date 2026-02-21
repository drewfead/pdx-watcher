package scraper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func indentJSON(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// writeGoldenFiles creates goldenDir and writes each map entry as a pretty-printed JSON file.
func writeGoldenFiles(goldenDir string, files map[string][]byte) error {
	if err := os.MkdirAll(goldenDir, 0o750); err != nil {
		return fmt.Errorf("failed to create golden dir: %w", err)
	}
	for key, body := range files {
		pretty, err := indentJSON(body)
		if err != nil {
			return fmt.Errorf("failed to format %s golden file: %w", key, err)
		}
		if err := os.WriteFile(filepath.Join(goldenDir, key+".json"), pretty, 0o600); err != nil {
			return fmt.Errorf("failed to write %s golden file: %w", key, err)
		}
	}
	return nil
}
