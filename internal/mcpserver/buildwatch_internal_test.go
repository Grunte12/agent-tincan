package mcpserver

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The watch reads a new build's version by running "<path> version",
// taking the last word printed on stdout or stderr.
func TestReadVersionRunsTheBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script")
	}
	path := filepath.Join(t.TempDir(), "tincan")
	script := "#!/bin/sh\n[ \"$1\" = version ] || exit 2\necho v0.7.0 >&2\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	v, err := readVersion(t.Context(), path)
	if err != nil || v != "v0.7.0" {
		t.Fatalf("readVersion = %q, %v", v, err)
	}
}
