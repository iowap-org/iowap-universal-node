package comfyui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/iowap-org/iowap-bridge-daemon/internal/provider"
)

// taskIDRe restricts the task-id path segment: relay task ids are
// opaque tokens; anything outside [A-Za-z0-9_.-] is rejected rather
// than escaped, so a hostile task_id can never add separators.
var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// flattenImages collects the image entries of one node's outputs block
// ({"9": {"images": [...]}, "12": {"gifs": [...]}, ...}).
func flattenImages(nodeOutputs any) []map[string]any {
	images := []map[string]any{}
	m, ok := nodeOutputs.(map[string]any)
	if !ok {
		return images
	}
	for _, v := range m {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		for _, entry := range list {
			if img, ok := entry.(map[string]any); ok {
				if _, has := img["filename"]; has {
					images = append(images, img)
				}
			}
		}
	}
	return images
}

// buildRelPath builds the workspace-relative path for one output:
// <taskID>/<subfolder>/<filename>, with every segment sanitized. It
// returns ok=false when any segment could escape the workspace.
func buildRelPath(taskID, subfolder, filename string) (string, bool) {
	if filename == "" {
		return "", false
	}
	segments := []string{}
	if taskID != "" {
		if !taskIDRe.MatchString(taskID) {
			return "", false
		}
		segments = append(segments, taskID)
	}
	for _, seg := range []string{subfolder, filename} {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		cleaned, ok := provider.SanitizeRelPath(seg)
		if !ok || strings.Contains(cleaned, "/") {
			// A subfolder/filename must be a single segment; slash
			// (or "..") inside it means we do not control the target.
			return "", false
		}
		segments = append(segments, cleaned)
	}
	if len(segments) == 0 {
		return "", false
	}
	return strings.Join(segments, "/"), true
}

// writeWorkspaceFile writes data to workspaceDir/<rel>, creating parent
// directories. rel must have passed buildRelPath/provider.SanitizeRelPath.
func writeWorkspaceFile(workspaceDir, rel string, data []byte) error {
	target := filepath.Join(workspaceDir, filepath.FromSlash(rel))
	// Defense in depth: re-verify containment after slash conversion.
	if !filepath.IsAbs(target) {
		abs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		target = abs
	}
	absWS, err := filepath.Abs(workspaceDir)
	if err != nil {
		return err
	}
	relToWS, err := filepath.Rel(absWS, target)
	if err != nil || relToWS == ".." || filepath.IsAbs(relToWS) || hasDotDotPrefix(relToWS) {
		return fmt.Errorf("artifact path %q escapes the workspace", rel)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o644)
}

func hasDotDotPrefix(rel string) bool {
	return rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(os.PathSeparator)
}

// truncate shortens s for error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
