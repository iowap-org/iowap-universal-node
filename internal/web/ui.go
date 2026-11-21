// Package web serves the daemon's local onboarding UI (board T-010): a
// small stdlib html/template page on 127.0.0.1:7782 that shows the node
// container status, offers the node registration form (server field
// pre-filled from the container's mDNS relay discovery) and — exactly once
// after bootstrap — the MCP bearer with ready-made agent config snippets
// (board T-014, SPEC §9).
package web

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/iowap-org/iowap-bridge-daemon/internal/bootstrap"
	"github.com/iowap-org/iowap-bridge-daemon/internal/nodecli"
	"github.com/iowap-org/iowap-bridge-daemon/internal/provision"
)

// EngineStatus is the container-status view the UI needs.
type EngineStatus interface {
	ContainerRunning(ctx context.Context, name string) (bool, error)
}

// ContainerExecer runs a command inside the node container.
type ContainerExecer interface {
	Exec(ctx context.Context, container string, cmd []string, timeout time.Duration) (nodecli.Result, error)
}

// UI is the onboarding web UI.
type UI struct {
	engine    EngineStatus
	execer    ContainerExecer
	container string
	view      *provision.OnceView
	mcpPort   int
	tmpl      *template.Template
}

// New wires the UI. view may be nil (daemon found the container already
// running — nothing to display).
func New(engine EngineStatus, execer ContainerExecer, view *provision.OnceView, mcpPort int) *UI {
	return &UI{
		engine:    engine,
		execer:    execer,
		container: bootstrap.ContainerName,
		view:      view,
		mcpPort:   mcpPort,
		tmpl:      template.Must(template.New("page").Parse(pageTemplate)),
	}
}

// pageData is the template model.
type pageData struct {
	ContainerStatus string
	ServerPrefill   string
	Register        *registerView
	Bearer          bearerView
	MCPPort         int
}

type registerView struct {
	Attempted bool
	OK        bool
	Message   string
}

type bearerView struct {
	Reveal   bool
	Bearer   string
	Snippets map[string]string
	Hash     string
	Note     string
}

// Handler returns the UI routes (GET /, POST /register).
func (u *UI) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", u.handleIndex)
	mux.HandleFunc("POST /register", u.handleRegister)
	return mux
}

func (u *UI) handleIndex(w http.ResponseWriter, r *http.Request) {
	u.render(w, pageData{ServerPrefill: u.discoverServer(r.Context())})
}

func (u *UI) handleRegister(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	server := strings.TrimSpace(r.FormValue("server"))

	var reg *registerView
	switch {
	case name == "" || server == "":
		reg = &registerView{Attempted: true, Message: "Both fields are required: node name and relay server (host:port)."}
	default:
		reg = u.register(r.Context(), name, server)
	}

	u.render(w, pageData{ServerPrefill: server, Register: reg})
}

func (u *UI) register(ctx context.Context, name, server string) *registerView {
	if u.execer == nil {
		return &registerView{Attempted: true, Message: "node container not available — cannot run the register command"}
	}
	res, err := u.execer.Exec(ctx, u.container, nodecli.RegisterArgv(server, name), nodecli.RegisterTimeout)
	if err != nil {
		return &registerView{Attempted: true, Message: "register command failed: " + err.Error()}
	}
	outcome := nodecli.ParseRegister(res)
	if !outcome.OK {
		return &registerView{Attempted: true, Message: "Registration failed: " + outcome.Message}
	}
	return &registerView{Attempted: true, OK: true, Message: "Node registriert — Heartbeat läuft"}
}

// discoverServer runs the mDNS relay discovery inside the container and
// returns the relay URL, or "" when nothing was found / discovery failed
// (the user then types the server manually).
func (u *UI) discoverServer(ctx context.Context) string {
	if u.execer == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, nodecli.DiscoverTimeout)
	defer cancel()
	res, err := u.execer.Exec(ctx, u.container, nodecli.DiscoverArgv(), nodecli.DiscoverTimeout)
	if err != nil {
		log.Printf("relay discovery unavailable: %v", err)
		return ""
	}
	return nodecli.ParseDiscoverURL(res.Stdout)
}

func (u *UI) render(w http.ResponseWriter, data pageData) {
	data.ContainerStatus = u.containerStatus(context.Background())
	data.Bearer = u.bearerSection()
	data.MCPPort = u.mcpPort
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := u.tmpl.Execute(w, data); err != nil {
		log.Printf("rendering onboarding page: %v", err)
	}
}

func (u *UI) containerStatus(ctx context.Context) string {
	if u.engine == nil {
		return "unknown (no docker engine connection)"
	}
	running, err := u.engine.ContainerRunning(ctx, u.container)
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	if running {
		return "running"
	}
	return "not running"
}

// bearerSection implements the one-time display (SPEC §9): the very first
// render carries the plaintext bearer plus the four config snippets and
// marks them displayed; every later render carries only the SHA-256 hash.
func (u *UI) bearerSection() bearerView {
	if u.view == nil || !u.view.HasBearer() {
		return bearerView{Note: "No MCP bearer was generated in this daemon run (the node container was already running when the daemon started)."}
	}
	if bearer, snippets, ok := u.view.Display(u.mcpPort); ok {
		return bearerView{Reveal: true, Bearer: bearer, Snippets: snippets}
	}
	return bearerView{Hash: u.view.Hash(), Note: u.view.Note()}
}

const pageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>iowap bridge — onboarding</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 2rem auto; max-width: 46rem; color: #1c1c1c; }
  h1 { font-size: 1.3rem; } h2 { font-size: 1.05rem; margin-top: 2rem; }
  .status { font-weight: 600; }
  label { display: block; margin-top: 0.8rem; font-weight: 600; }
  input[type=text] { width: 100%; padding: 0.45rem; margin-top: 0.2rem; box-sizing: border-box; }
  button { margin-top: 1rem; padding: 0.5rem 1.2rem; }
  .ok { color: #0a6b2d; font-weight: 600; }
  .err { color: #a02020; font-weight: 600; }
  pre { background: #f4f4f4; padding: 0.7rem; border-radius: 6px; overflow-x: auto; }
  .note { color: #555; font-size: 0.9rem; }
</style>
</head>
<body>
<h1>iowap bridge daemon — node onboarding</h1>

<p>Node container <code>{{.ContainerStatus}}</code></p>

<h2>Register this node on a relay</h2>
<form method="post" action="/register">
  <label for="name">Node name</label>
  <input type="text" id="name" name="name" placeholder="e.g. workshop-pc" value="">
  <label for="server">Relay server (host:port or URL)</label>
  <input type="text" id="server" name="server" placeholder="e.g. 192.0.2.10:8788" value="{{.ServerPrefill}}">
  <button type="submit">Register</button>
</form>
{{if .Register}}
  {{if .Register.OK}}<p class="ok">{{.Register.Message}}</p>
  {{else}}<p class="err">{{.Register.Message}}</p>{{end}}
{{end}}

<h2>HTTP MCP endpoint — agent access</h2>
{{if .Bearer.Reveal}}
  <p>Copy the snippets below now — the daemon shows the bearer <strong>only this once</strong> and keeps just its hash afterwards.</p>
  <p><strong>Bearer:</strong> <code>{{.Bearer.Bearer}}</code></p>
  <h3>Hermes</h3>
  <pre>{{index .Bearer.Snippets "hermes"}}</pre>
  <h3>Claude Desktop / Claude Code</h3>
  <pre>{{index .Bearer.Snippets "claude"}}</pre>
  <h3>Codex (config.toml)</h3>
  <pre>{{index .Bearer.Snippets "codex"}}</pre>
  <h3>Live probe (generic)</h3>
  <pre>{{index .Bearer.Snippets "curl"}}</pre>
{{else}}
  {{if .Bearer.Hash}}
    <p>Bearer hash (sha256): <code>{{.Bearer.Hash}}</code></p>
    <p class="note">{{.Bearer.Note}}</p>
  {{else}}
    <p class="note">{{.Bearer.Note}}</p>
  {{end}}
{{end}}

<p class="note">Bridge API on port 7780 (manifest/exec, loopback only) · Onboarding UI on port 7782 (this page, loopback only) · MCP endpoint on port {{.MCPPort}} (loopback only)</p>
</body>
</html>`
