package nodecli

import (
	"reflect"
	"testing"
)

func TestDiscoverArgv(t *testing.T) {
	got := DiscoverArgv()
	want := []string{"iowap-cli", "--json", "relay", "discover", "--timeout", "3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverArgv() = %v, want %v", got, want)
	}
}

func TestRegisterArgv(t *testing.T) {
	got := RegisterArgv("192.0.2.10:8788", "workshop-pc")
	want := []string{"iowap-cli", "--json", "node", "register", "192.0.2.10:8788", "--name", "workshop-pc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RegisterArgv() = %v, want %v", got, want)
	}
}

func TestParseDiscoverURL(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{"full hit", "http://192.0.2.60:8788\n  service: IOWAP Relay Service (_http._tcp)\n", "http://192.0.2.60:8788"},
		{"url only", "http://192.0.2.60:8788\n", "http://192.0.2.60:8788"},
		{"https", "https://relay.example.com:8443\n", "https://relay.example.com:8443"},
		{"not found banner", "relay not found: no mDNS service named 'IOWAP Relay Service' (watched 2s)\n", ""},
		{"empty stdout", "", ""},
		{"only whitespace", "\n  \n", ""},
		{"log noise around url", "INFO: watching for services\nhttp://192.0.2.60:8788\n  service: x\n", "http://192.0.2.60:8788"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseDiscoverURL(tt.stdout); got != tt.want {
				t.Fatalf("ParseDiscoverURL(%q) = %q, want %q", tt.stdout, got, tt.want)
			}
		})
	}
}

func TestParseDiscoverURLRejectsNonURLLines(t *testing.T) {
	// A "not found" diagnostic must never be mistaken for a URL even when
	// it contains an http:// mention mid-line.
	if got := ParseDiscoverURL("relay not found: tried http://none.local\n"); got != "" {
		t.Fatalf("ParseDiscoverURL accepted a diagnostic line: %q", got)
	}
}

func TestParseRegisterSuccess(t *testing.T) {
	res := Result{
		Stdout:   `{"ok": true, "node_id": "nd_123", "node_name": "workshop-pc", "status": "pending"}`,
		ExitCode: 0,
	}
	got := ParseRegister(res)
	if !got.OK {
		t.Fatalf("expected success, got %+v", got)
	}
	if got.NodeID != "nd_123" {
		t.Fatalf("NodeID = %q, want nd_123", got.NodeID)
	}
}

func TestParseRegisterFailureExitCode(t *testing.T) {
	res := Result{
		Stderr:   "registration failed: HTTP 500: boom",
		ExitCode: 1,
	}
	got := ParseRegister(res)
	if got.OK {
		t.Fatal("expected failure")
	}
	if got.Message != "registration failed: HTTP 500: boom" {
		t.Fatalf("Message = %q", got.Message)
	}
}

func TestParseRegisterFailureNoOutput(t *testing.T) {
	got := ParseRegister(Result{ExitCode: 2})
	if got.OK {
		t.Fatal("expected failure")
	}
	if got.Message == "" {
		t.Fatal("expected a non-empty failure message")
	}
}

func TestParseRegisterExitZeroWithoutSuccessJSON(t *testing.T) {
	got := ParseRegister(Result{Stdout: "something odd", ExitCode: 0})
	if got.OK {
		t.Fatal("exit 0 without ok-JSON must not count as success")
	}
}

func TestParseRegisterJSONInLogNoise(t *testing.T) {
	res := Result{
		Stdout:   "WARNING: log line\n{\"ok\": true, \"node_id\": \"nd_9\"}\n",
		ExitCode: 0,
	}
	if got := ParseRegister(res); !got.OK || got.NodeID != "nd_9" {
		t.Fatalf("expected success nd_9, got %+v", got)
	}
}

func TestParseRegisterFailsOnExplicitFalse(t *testing.T) {
	res := Result{Stdout: `{"ok": false}`, ExitCode: 0}
	if got := ParseRegister(res); got.OK {
		t.Fatal(`{"ok": false} must not count as success`)
	}
}
