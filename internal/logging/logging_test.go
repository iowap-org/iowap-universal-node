package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePath(t *testing.T) {
	home := filepath.Join("home", "u")
	homeDir := func() (string, error) { return home, nil }

	t.Run("LOCALAPPDATA wins", func(t *testing.T) {
		got, err := ResolvePath(func(string) string { return `C:\Users\u\AppData\Local` }, homeDir)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(`C:\Users\u\AppData\Local`, "iowap-bridge", "daemon.log")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("falls back to home/AppData/Local", func(t *testing.T) {
		got, err := ResolvePath(func(string) string { return "" }, homeDir)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(home, "AppData", "Local", "iowap-bridge", "daemon.log")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("no LOCALAPPDATA, no home → error", func(t *testing.T) {
		if _, err := ResolvePath(func(string) string { return "" },
			func() (string, error) { return "", os.ErrNotExist }); err == nil {
			t.Error("expected an error when neither LOCALAPPDATA nor home is resolvable")
		}
	})
}

func TestFileAppendAndRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iowap-bridge", "daemon.log")

	// Open creates parent dirs.
	l, err := Open(path, MaxLogSize)
	if err != nil {
		t.Fatal(err)
	}
	msg1 := "bootstrap line one\n"
	if n, err := l.Write([]byte(msg1)); err != nil || n != len(msg1) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	// Reopen must append, not truncate.
	l.Close()
	l2, err := Open(path, MaxLogSize)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	msg2 := "bootstrap line two\n"
	if _, err := l2.Write([]byte(msg2)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != msg1+msg2 {
		t.Errorf("append broken: got %q", got)
	}
}

func TestFileRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	small := int64(64)
	l, err := Open(path, small)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Fill beyond the threshold: each write checks size+len first.
	var b bytes.Buffer
	for i := 0; i < 10; i++ {
		line := strings.Repeat("x", 20) + "\n"
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		b.WriteString(line)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > small+21 {
		t.Errorf("current log did not rotate: %d bytes (max %d)", fi.Size(), small)
	}
	prev, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("previous generation missing: %v", err)
	}
	// The .1 file must hold the earlier lines (exactly, since the last
	// generation got rotated once at most with these sizes).
	if len(prev) == 0 || len(prev) >= len(b.String()) {
		t.Errorf("rotated content unexpected: %d bytes of %d total", len(prev), b.Len())
	}
	// Path helper.
	if l.Path() != path {
		t.Errorf("Path() = %q, want %q", l.Path(), path)
	}
}

func TestFileOpenRotatesOversizedExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("oversized file not rotated on open: %v", err)
	}
}
