package handlers

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Regression: o candidato era executado sem argumentos (REPL + EOF, sem
// output) e o regex de versão nunca batia — "Python >= 3.10 not found" mesmo
// com python3.13 no PATH. A detecção usa `--version`, como a origem.
func TestFindPython310DetectsVersionOutput(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "python3.13")
	if runtime.GOOS == "windows" {
		script += ".cmd"
	}
	body := "#!/bin/sh\necho 'Python 3.13.5'\n"
	if runtime.GOOS == "windows" {
		body = "@echo Python 3.13.5\r\n"
	}
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := findPython310(); got == "" {
		t.Fatal("findPython310 não detectou python com output de versão")
	}
}
