package provision

import (
	"encoding/json"
	"strings"
	"testing"
)

const testBearer = "iw_mcp_GOLDEN_BEARER_NOT_A_REAL_TOKEN_0123456789abcdefghij"

func TestSnippetsHermesGolden(t *testing.T) {
	got := Snippets(testBearer, DefaultMCPPort)["hermes"]
	want := `hermes mcp add iowap --url http://127.0.0.1:7781/mcp --header "Authorization: Bearer iw_mcp_GOLDEN_BEARER_NOT_A_REAL_TOKEN_0123456789abcdefghij"`
	if got != want {
		t.Fatalf("hermes snippet mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestSnippetsClaudeGolden(t *testing.T) {
	got := Snippets(testBearer, DefaultMCPPort)["claude"]
	want := `{
  "mcpServers": {
    "iowap": {
      "type": "http",
      "url": "http://127.0.0.1:7781/mcp",
      "headers": { "Authorization": "Bearer iw_mcp_GOLDEN_BEARER_NOT_A_REAL_TOKEN_0123456789abcdefghij" }
    }
  }
}`
	if got != want {
		t.Fatalf("claude snippet mismatch:\n got  %s\n want %s", got, want)
	}
	// The snippet is the exact config fragment agents consume — it must
	// stay valid JSON.
	if !json.Valid([]byte(got)) {
		t.Fatal("claude snippet is not valid JSON")
	}
}

func TestSnippetsCodexGolden(t *testing.T) {
	got := Snippets(testBearer, DefaultMCPPort)["codex"]
	want := `[mcp_servers.iowap]
url = "http://127.0.0.1:7781/mcp"
http_headers = { "Authorization" = "Bearer iw_mcp_GOLDEN_BEARER_NOT_A_REAL_TOKEN_0123456789abcdefghij" }`
	if got != want {
		t.Fatalf("codex snippet mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestSnippetsCurlGolden(t *testing.T) {
	got := Snippets(testBearer, DefaultMCPPort)["curl"]
	want := `curl -H "Authorization: Bearer iw_mcp_GOLDEN_BEARER_NOT_A_REAL_TOKEN_0123456789abcdefghij" http://127.0.0.1:7781/mcp`
	if got != want {
		t.Fatalf("curl snippet mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestSnippetsCustomPort(t *testing.T) {
	got := Snippets("iw_mcp_x", 9000)
	for name, snippet := range got {
		if !strings.Contains(snippet, "http://127.0.0.1:9000/mcp") {
			t.Fatalf("%s snippet ignores custom port: %s", name, snippet)
		}
	}
}

func TestSnippetsAllFourFormatsPresent(t *testing.T) {
	got := Snippets("iw_mcp_x", DefaultMCPPort)
	for _, name := range []string{"hermes", "claude", "codex", "curl"} {
		if got[name] == "" {
			t.Fatalf("missing %s snippet", name)
		}
		if !strings.Contains(got[name], "Bearer iw_mcp_x") {
			t.Fatalf("%s snippet lacks the bearer", name)
		}
	}
}

func TestOnceViewDisplaysExactlyOnce(t *testing.T) {
	v := NewOnceView("iw_mcp_abc")
	bearer, snippets, ok := v.Display(DefaultMCPPort)
	if !ok || bearer != "iw_mcp_abc" || snippets == nil {
		t.Fatalf("first display must carry the bearer, got ok=%v bearer=%q", ok, bearer)
	}

	// After the one-time display only the hash remains.
	bearer, snippets, ok = v.Display(DefaultMCPPort)
	if ok || bearer != "" || snippets != nil {
		t.Fatalf("second display must be empty, got ok=%v bearer=%q", ok, bearer)
	}
	if !v.Shown() {
		t.Fatal("Shown() must be true after display")
	}
	if want := HashBearer("iw_mcp_abc"); v.Hash() != want {
		t.Fatalf("Hash() = %q, want %q", v.Hash(), want)
	}
	if !strings.Contains(v.Note(), "rotation = container recreate") {
		t.Fatalf("Note() = %q, want the rotation hint", v.Note())
	}
}

func TestOnceViewNilAndEmptySafe(t *testing.T) {
	var v *OnceView
	if _, _, ok := v.Display(7781); ok {
		t.Fatal("nil view must not display")
	}
	if v.Hash() != "" || v.Shown() != true {
		t.Fatal("nil view must behave like already-displayed/empty")
	}
	empty := NewOnceView("")
	if empty.HasBearer() {
		t.Fatal("empty view must report HasBearer()=false")
	}
}
