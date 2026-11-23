// Package logging provides the file logging used when the console window
// is hidden (v0.3.1): bootstrap/tray/watch messages additionally go to
// daemon.log next to the config dir so a double-click start (no console,
// windowsgui subsystem) stays debuggable. Rotation is deliberately simple:
// at MaxLogSize the file is renamed to <name>.1 (overwriting the previous
// one) and reopened — no third generation, no locking.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
)

// MaxLogSize is the rotation threshold (board T-023 feedback round: simple
// 5 MB rotation, append mode).
const MaxLogSize = 5 * 1024 * 1024

// ResolvePath returns the daemon.log path:
// %LOCALAPPDATA%\iowap-bridge\daemon.log. When LOCALAPPDATA is unset
// (non-Windows, service contexts) it falls back to <home>/AppData/Local —
// the same directory os.UserConfigDir's sibling, kept explicit and
// injectable so the resolution stays testable (config.Load style).
func ResolvePath(getenv func(string) string, homeDir func() (string, error)) (string, error) {
	base := getenv("LOCALAPPDATA")
	if base == "" {
		home, err := homeDir()
		if err != nil {
			return "", fmt.Errorf("log dir: no LOCALAPPDATA and no home dir: %w", err)
		}
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "iowap-bridge", "daemon.log"), nil
}

// File is the append-only, size-capped log file. It is an io.Writer and
// safe for the sequential use log.SetOutput makes of it.
type File struct {
	f        *os.File
	path     string
	maxBytes int64
}

// Open creates the log dir if needed and opens (or appends to) the log
// file. An existing file larger than maxBytes is rotated immediately so a
// fresh run always starts with a small file.
func Open(path string, maxBytes int64) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("log dir: %w", err)
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxBytes {
		rotate(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &File{f: f, path: path, maxBytes: maxBytes}, nil
}

// Write appends p, rotating the file first when the threshold is exceeded.
// A rotation failure never loses the message: the write then goes to the
// still-open old file.
func (l *File) Write(p []byte) (int, error) {
	if l.needsRotate(len(p)) {
		l.f.Close() //nolint:errcheck // rotation below reopens; failure keeps the old handle
		if err := rotate(l.path); err == nil {
			if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				l.f = f
			}
			// reopen failed → f stays closed; Write below reports the error
		}
	}
	n, err := l.f.Write(p)
	if err != nil {
		return n, fmt.Errorf("write %s: %w", l.path, err)
	}
	return n, nil
}

// Close closes the underlying file.
func (l *File) Close() error { return l.f.Close() }

// Path returns the log file path (exposed for diagnostics/tests).
func (l *File) Path() string { return l.path }

func (l *File) needsRotate(n int) bool {
	fi, err := l.f.Stat()
	return err == nil && fi.Size()+int64(n) > l.maxBytes
}

// rotate renames path to path+".1", replacing any previous generation.
func rotate(path string) error {
	if err := os.Remove(path + ".1"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate %s: %w", path, err)
	}
	if err := os.Rename(path, path+".1"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate %s: %w", path, err)
	}
	return nil
}
