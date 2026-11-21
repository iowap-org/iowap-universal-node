package provision

import (
	"fmt"
	"strings"
)

// DefaultMCPPort is the loopback port the universal-node compose profile
// publishes the HTTP MCP endpoint on (SPEC §9 default suggestion).
const DefaultMCPPort = 7781

// Snippets renders the four agent config snippets for the given bearer and
// MCP port, exactly in the formats from SPEC §9. Keys: "hermes", "claude",
// "codex", "curl".
func Snippets(bearer string, mcpPort int) map[string]string {
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", mcpPort)
	auth := "Bearer " + bearer
	return map[string]string{
		"hermes": fmt.Sprintf(`hermes mcp add iowap --url %s --header "Authorization: %s"`, url, auth),
		"claude": fmt.Sprintf(`{
  "mcpServers": {
    "iowap": {
      "type": "http",
      "url": "%s",
      "headers": { "Authorization": "%s" }
    }
  }
}`, url, auth),
		"codex": fmt.Sprintf(`[mcp_servers.iowap]
url = "%s"
http_headers = { "Authorization" = "%s" }`, url, auth),
		"curl": fmt.Sprintf(`curl -H "Authorization: %s" %s`, auth, url),
	}
}

// OnceView holds the bearer in memory until it has been displayed exactly
// once; afterwards only the SHA-256 hash remains (rotation = container
// recreate, SPEC §9). Not persisted — a daemon restart after the container
// already exists simply has no bearer to show.
type OnceView struct {
	bearer string
	hash   string
	shown  bool
}

// NewOnceView stores a freshly generated bearer.
func NewOnceView(bearer string) *OnceView {
	return &OnceView{bearer: bearer, hash: HashBearer(bearer)}
}

// Display returns the plaintext bearer and the snippets exactly once.
// Every call after the first returns ok=false; the caller shows the hash
// instead.
func (v *OnceView) Display(mcpPort int) (bearer string, snippets map[string]string, ok bool) {
	if v == nil || v.shown {
		return "", nil, false
	}
	v.shown = true
	return v.bearer, Snippets(v.bearer, mcpPort), true
}

// Hash returns the hex SHA-256 the daemon keeps after the display.
func (v *OnceView) Hash() string {
	if v == nil {
		return ""
	}
	return v.hash
}

// Shown reports whether the one-time display already happened.
func (v *OnceView) Shown() bool {
	return v == nil || v.shown
}

// HasBearer reports whether this view ever held a bearer (i.e. this daemon
// run performed the bootstrap). A daemon that found the container already
// running never had one.
func (v *OnceView) HasBearer() bool {
	return v != nil && v.bearer != ""
}

// Note returns the rotation hint shown next to the hash.
func (v *OnceView) Note() string {
	return strings.TrimSpace("Already displayed — rotation = container recreate")
}
