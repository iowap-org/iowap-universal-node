package tray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Version comparison (exe update check) ----------------------------------

func TestUpdateAvailableMatrix(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
		wantErr         bool
	}{
		{"0.2.1", "0.2.1", false, false},
		{"v0.2.1", "v0.3.0", true, false},
		{"0.3.0", "0.2.1", false, false},
		{"0.10.0", "0.9.9", false, false}, // numeric, not lexicographic
		{"0.2.1", "1.0.0", true, false},
		{"0.2.1", "", false, true},  // no release found → unknown
		{"", "0.3.0", false, false}, // dev build (empty version) → never claims update
		{"0.2.1", "not-a-version", false, true},
	}
	for _, tc := range cases {
		got, err := UpdateAvailable(tc.current, tc.latest)
		if (err != nil) != tc.wantErr {
			t.Errorf("UpdateAvailable(%q, %q) err = %v, wantErr %v", tc.current, tc.latest, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("UpdateAvailable(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

// --- GitHub latest release (exe update check) -------------------------------

func TestLatestReleaseTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/iowap-org/iowap-bridge-daemon/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name": "v0.4.0", "name": "v0.4.0"}`))
	}))
	defer srv.Close()

	tag, err := latestReleaseFrom(context.Background(), srv.Client(), srv.URL, "iowap-org/iowap-bridge-daemon")
	if err != nil {
		t.Fatalf("latestReleaseFrom: %v", err)
	}
	if tag != "v0.4.0" {
		t.Fatalf("tag = %q, want v0.4.0", tag)
	}
}

func TestLatestReleaseTagServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := latestReleaseFrom(context.Background(), srv.Client(), srv.URL, "org/repo"); err == nil {
		t.Fatal("HTTP 403 must surface as an error (shown in the tray status, not swallowed)")
	}
}

// --- Registry v2 digest (container image update check) ----------------------

func TestRemoteImageDigest(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:org/image:pull" {
				t.Errorf("token scope = %q", r.URL.Query().Get("scope"))
			}
			w.Write([]byte(`{"token": "tok123"}`))
		case r.URL.Path == "/v2/org/image/manifests/latest":
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Docker-Content-Digest", "sha256:deadbeef")
			w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	reg := registryEndpoints{Base: srv.URL, TokenURL: srv.URL + "/token?scope=repository:org/image:pull"}
	digest, err := remoteDigest(context.Background(), srv.Client(), reg, "org/image", "latest")
	if err != nil {
		t.Fatalf("remoteDigest: %v", err)
	}
	if digest != "sha256:deadbeef" {
		t.Fatalf("digest = %q, want sha256:deadbeef", digest)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("manifest Authorization = %q, want Bearer tok123", gotAuth)
	}
}

func TestRemoteImageDigestMissingHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Write([]byte(`{"token": "t"}`))
			return
		}
		w.Write([]byte("{}")) // no Docker-Content-Digest
	}))
	defer srv.Close()

	reg := registryEndpoints{Base: srv.URL, TokenURL: srv.URL + "/token"}
	if _, err := remoteDigest(context.Background(), srv.Client(), reg, "org/image", "latest"); err == nil {
		t.Fatal("manifest without Docker-Content-Digest must be an error")
	}
}

// The ghcr.io URL builder: image ref → registry + token endpoints.
func TestRegistryEndpointsForGHCR(t *testing.T) {
	reg, repo, tag, err := parseImageRef("ghcr.io/iowap-org/iowap-universal-node:latest")
	if err != nil {
		t.Fatalf("parseImageRef: %v", err)
	}
	if reg.Base != "https://ghcr.io" {
		t.Errorf("base = %q", reg.Base)
	}
	if !strings.Contains(reg.TokenURL, "https://ghcr.io/token") {
		t.Errorf("token URL = %q", reg.TokenURL)
	}
	if !strings.Contains(reg.TokenURL, "scope=repository:iowap-org/iowap-universal-node:pull") {
		t.Errorf("token URL scope missing: %q", reg.TokenURL)
	}
	if repo != "iowap-org/iowap-universal-node" || tag != "latest" {
		t.Errorf("repo/tag = %q/%q", repo, tag)
	}
}

// Local digest extraction from docker RepoDigests.
func TestLocalImageDigest(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"ghcr.io/iowap-org/iowap-universal-node@sha256:aaa"}, "sha256:aaa"},
		{[]string{"other@sha256:bbb", "ghcr.io/iowap-org/iowap-universal-node@sha256:aaa"}, "sha256:aaa"},
		{nil, ""},
		{[]string{"malformed"}, ""},
	}
	for _, tc := range cases {
		if got := localImageDigest("ghcr.io/iowap-org/iowap-universal-node", tc.in); got != tc.want {
			t.Errorf("localImageDigest(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- Image update decision ---------------------------------------------------

func TestImageUpdateAvailable(t *testing.T) {
	cases := []struct {
		name        string
		repoDigests []string
		remote      string
		want        bool
		wantErr     bool
	}{
		{"same digest", []string{"ghcr.io/x@sha256:aaa"}, "sha256:aaa", false, false},
		{"remote newer", []string{"ghcr.io/x@sha256:aaa"}, "sha256:bbb", true, false},
		{"no local digest", nil, "sha256:bbb", false, true}, // cannot compare → unknown
		{"no remote digest", []string{"ghcr.io/x@sha256:aaa"}, "", false, true},
	}
	for _, tc := range cases {
		got, err := ImageUpdateAvailable("ghcr.io/x", tc.repoDigests, tc.remote)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
