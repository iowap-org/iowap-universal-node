// Update checks (board T-023): (a) container image — registry digest
// comparison latest vs. local (docker RepoDigests); (b) the daemon exe —
// GitHub latest release of the daemon repo vs. the running version.
// Both are INFORMATIONAL in v1: the tray reports "update available",
// nothing is downloaded or executed.
package tray

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GitHubRepo is the daemon's own repository (exe update check).
const GitHubRepo = "iowap-org/iowap-bridge-daemon"

// NodeImage is the universal-node image the vendored compose profile runs.
const NodeImage = "ghcr.io/iowap-org/iowap-node-base:latest"

// UpdateAvailable reports whether the latest release tag is newer than
// the running version (semantic version comparison; a "v" prefix is
// tolerated on both sides). Errors surface to the tray status instead of
// silently claiming "up to date".
func UpdateAvailable(current, latest string) (bool, error) {
	cur, err := parseVersion(current)
	if err != nil {
		return false, fmt.Errorf("current version %q: %w", current, err)
	}
	if cur.unset {
		// Dev build without a version: never claim an update.
		return false, nil
	}
	lat, err := parseVersion(latest)
	if err != nil {
		return false, fmt.Errorf("latest release tag %q: %w", latest, err)
	}
	if lat.unset {
		return false, fmt.Errorf("no release information available")
	}
	if lat.major != cur.major {
		return lat.major > cur.major, nil
	}
	if lat.minor != cur.minor {
		return lat.minor > cur.minor, nil
	}
	return lat.patch > cur.patch, nil
}

type version struct {
	major, minor, patch int
	unset               bool
}

func parseVersion(s string) (version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return version{unset: true}, nil
	}
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return version{}, fmt.Errorf("not a semantic version")
	}
	v := version{}
	nums := [3]int{}
	for i, p := range parts {
		if p == "" {
			return version{}, fmt.Errorf("empty version segment")
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}, fmt.Errorf("segment %q is not a number", p)
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	return v, nil
}

// latestReleaseFrom queries the GitHub releases API (injectable base URL
// for tests; LatestReleaseTag is the production wrapper).
func latestReleaseFrom(ctx context.Context, hc *http.Client, apiBase, repo string) (string, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	url := apiBase + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("querying GitHub releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub releases API returned %d for %s", resp.StatusCode, url)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding GitHub release: %w", err)
	}
	return strings.TrimSpace(body.TagName), nil
}

// LatestReleaseTag fetches the latest release tag of the daemon repo.
func LatestReleaseTag(ctx context.Context, hc *http.Client, repo string) (string, error) {
	return latestReleaseFrom(ctx, hc, "https://api.github.com", repo)
}

// registryEndpoints locates the registry v2 API and its anonymous token
// endpoint for an image reference's registry host.
type registryEndpoints struct {
	Base     string // https://ghcr.io
	TokenURL string // full token URL template with scope already set
}

// parseImageRef splits an image reference "host/repo[:tag]" and returns
// the registry endpoints, repo path and tag. Supported: ghcr.io,
// docker.io (→ index.docker.io) and generic hosts.
func parseImageRef(ref string) (registryEndpoints, string, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return registryEndpoints{}, "", "", fmt.Errorf("empty image reference")
	}
	repo, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	host, path := "docker.io", repo
	if i := strings.Index(repo, "/"); i > 0 && (strings.ContainsAny(repo[:i], ".:") || repo[:i] == "localhost") {
		host, path = repo[:i], repo[i+1:]
	}
	if host == "docker.io" || host == "index.docker.io" {
		if !strings.Contains(path, "/") {
			path = "library/" + path
		}
		return registryEndpoints{
			Base:     "https://index.docker.io",
			TokenURL: "https://auth.docker.io/token?service=registry.docker.io&scope=repository:" + path + ":pull",
		}, path, tag, nil
	}
	return registryEndpoints{
		Base:     "https://" + host,
		TokenURL: "https://" + host + "/token?scope=repository:" + path + ":pull",
	}, path, tag, nil
}

// remoteDigest resolves an image tag's current digest via the registry
// v2 API (anonymous pull token, manifest HEAD semantics via GET with
// Docker-Content-Digest). Injectable endpoints for tests.
func remoteDigest(ctx context.Context, hc *http.Client, reg registryEndpoints, repo, tag string) (string, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	token, err := registryToken(ctx, hc, reg.TokenURL)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.Base+"/v2/"+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	for _, accept := range []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	} {
		req.Header.Add("Accept", accept)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("querying %s manifest: %w", reg.Base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry returned %d for %s:%s", resp.StatusCode, repo, tag)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("registry did not report Docker-Content-Digest for %s:%s", repo, tag)
	}
	return digest, nil
}

func registryToken(ctx context.Context, hc *http.Client, tokenURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Registries without an anonymous token endpoint: try anonymous.
		return "", nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", nil
	}
	return body.Token, nil
}

// RemoteImageDigest is the production entry point: image ref → registry digest.
func RemoteImageDigest(ctx context.Context, hc *http.Client, imageRef string) (string, error) {
	reg, repo, tag, err := parseImageRef(imageRef)
	if err != nil {
		return "", err
	}
	return remoteDigest(ctx, hc, reg, repo, tag)
}

// localImageDigest picks the repo digest matching imageRepo from docker
// RepoDigests entries ("ghcr.io/org/img@sha256:..."). Empty when none match.
func localImageDigest(imageRepo string, repoDigests []string) string {
	for _, rd := range repoDigests {
		if i := strings.Index(rd, "@"); i >= 0 && rd[:i] == imageRepo {
			return rd[i+1:]
		}
	}
	return ""
}

// ImageUpdateAvailable compares the locally pulled digests of imageRepo
// against the registry's current digest. Unknown states (nothing pulled
// yet, no remote answer) are errors — the tray shows "cannot check", it
// never shows a false "up to date".
func ImageUpdateAvailable(imageRepo string, localRepoDigests []string, remoteDigest string) (bool, error) {
	local := localImageDigest(imageRepo, localRepoDigests)
	if local == "" {
		return false, fmt.Errorf("no local digest known for %s", imageRepo)
	}
	if remoteDigest == "" {
		return false, fmt.Errorf("no remote digest for %s", imageRepo)
	}
	return local != remoteDigest, nil
}
