// Package envs loads KEY=VALUE lines from a .env-style file into the
// process environment. Minimal stdlib substitute for a .env-loading
// dependency; existing environment variables always win.
package envs

import (
	"bufio"
	"os"
	"strings"
)

// Load reads path (if it exists) and calls os.Setenv for each KEY=VALUE
// line, skipping blanks, "# comment" lines, and any key already set in the
// environment. A missing file is not an error — callers that only want
// local-dev convenience can ignore it.
func Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		if _, set := os.LookupEnv(key); set {
			continue
		}
		os.Setenv(key, value)
	}
	return scanner.Err()
}
