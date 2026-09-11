package tracefile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWrite_RoundTripsJSON(t *testing.T) {
	t.Setenv("TRACE_FILE_DIR", t.TempDir())

	type payload struct {
		RequestID string
		Count     int
	}
	want := payload{RequestID: "REQ-1", Count: 3}

	path, err := Write("REQ-1", want)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if filepath.Base(path) != "REQ-1.json" {
		t.Errorf("path = %q, want to end in REQ-1.json", path)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	var got payload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshaling written file: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestWrite_CreatesDirAndOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "traces")
	t.Setenv("TRACE_FILE_DIR", dir)

	if _, err := Write("same-name", map[string]int{"v": 1}); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("Write didn't create %s: %v", dir, err)
	}

	path, err := Write("same-name", map[string]int{"v": 2})
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading overwritten file: %v", err)
	}
	var got map[string]int
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshaling overwritten file: %v", err)
	}
	if got["v"] != 2 {
		t.Errorf("v = %d, want 2 — a repeat Write of the same name should overwrite, not append or error", got["v"])
	}
}

func TestDir_DefaultsWhenUnset(t *testing.T) {
	t.Setenv("TRACE_FILE_DIR", "")
	if got := Dir(); got != "data/traces" {
		t.Errorf("Dir() = %q, want %q", got, "data/traces")
	}
}
