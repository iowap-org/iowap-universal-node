package openbrowser

import (
	"errors"
	"strings"
	"testing"
)

func TestCommandPerGOOS(t *testing.T) {
	cases := []struct {
		goos, wantBin string
		wantArgs      []string
	}{
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", "http://127.0.0.1:7783"}},
		{"darwin", "open", []string{"http://127.0.0.1:7783"}},
		{"linux", "xdg-open", []string{"http://127.0.0.1:7783"}},
	}
	for _, c := range cases {
		bin, args := Command(c.goos, "http://127.0.0.1:7783")
		if bin != c.wantBin || strings.Join(args, " ") != strings.Join(c.wantArgs, " ") {
			t.Errorf("Command(%q) = %q %v, want %q %v", c.goos, bin, args, c.wantBin, c.wantArgs)
		}
	}
}

func TestOpenWithLaunchesCommand(t *testing.T) {
	var gotBin string
	var gotArgs []string
	run := func(name string, args ...string) error {
		gotBin, gotArgs = name, args
		return nil
	}
	if err := OpenWith(run, "windows", "http://127.0.0.1:7783"); err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	if gotBin != "rundll32" || len(gotArgs) != 2 || gotArgs[1] != "http://127.0.0.1:7783" {
		t.Errorf("launched %q %v, want rundll32 with URL arg", gotBin, gotArgs)
	}
}

// The error is informational: OpenWith must return it (the caller prints
// the URL instead), and it must name the attempted command.
func TestOpenWithSurfacesErrors(t *testing.T) {
	run := func(name string, _ ...string) error { return errors.New("not found") }
	err := OpenWith(run, "linux", "http://127.0.0.1:7783")
	if err == nil {
		t.Fatal("OpenWith with failing runner = nil error, want error")
	}
	if !strings.Contains(err.Error(), "xdg-open") {
		t.Errorf("error %q does not name the command", err)
	}
	if err := OpenWith(run, "linux", ""); err == nil {
		t.Error("OpenWith with empty url = nil error, want error")
	}
}
