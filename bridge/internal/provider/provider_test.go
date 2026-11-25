package provider

import (
	"strings"
	"testing"
)

func TestSanitizeRelPath(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"out.png", "out.png", true},
		{"runs/2026-11-20/x/out.png", "runs/2026-11-20/x/out.png", true},
		{"a/b/../c.png", "a/c.png", true},
		{"", "", false},
		{"..", "", false},
		{"../evil.png", "", false},
		{"runs/../../etc/passwd", "", false},
		{"/etc/passwd", "", false},
		{`..\evil.png`, "", false},
		{`C:\temp\evil.png`, "", false},
		{"C:/temp/evil.png", "", false},
		{"./out.png", "out.png", true},
	}
	for _, c := range cases {
		got, ok := SanitizeRelPath(c.in)
		if ok != c.wantOK {
			t.Errorf("SanitizeRelPath(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && got != c.want {
			t.Errorf("SanitizeRelPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCapNamePattern(t *testing.T) {
	valid := []string{"comfy.generate", "ab", "host-x_1.y", strings.Repeat("a", 64)}
	invalid := []string{"", "a", "Comfy", "1abc", "-abc", "a b", strings.Repeat("a", 65)}
	for _, n := range valid {
		if !ValidCapName(n) {
			t.Errorf("ValidCapName(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if ValidCapName(n) {
			t.Errorf("ValidCapName(%q) = true, want false", n)
		}
	}
}
