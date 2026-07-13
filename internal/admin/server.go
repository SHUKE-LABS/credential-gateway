// Package admin serves a loopback-only web UI for viewing and editing the
// gateway's config file. It is a separate, unprivileged process (see
// cmd/credential-gateway-admin): anything able to write config.yaml can
// read/write every proxied credential, so the UI runs as a dedicated non-root
// user reaching exactly that one file via a POSIX ACL, binds 127.0.0.1 only,
// and has no way to restart or reload the gateway.
package admin

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"

	"credential-gateway/internal/config"
)

// Server serves and edits a single config file over HTTP.
type Server struct {
	configPath string
	log        *slog.Logger
	tmpl       *template.Template
}

// New returns a Server editing the file at configPath.
func New(configPath string, log *slog.Logger) *Server {
	return &Server{
		configPath: configPath,
		log:        log,
		tmpl:       template.Must(template.New("page").Parse(pageHTML)),
	}
}

// Handler returns the admin HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/save", s.handleSave)
	return mux
}

type pageData struct {
	Content string
	Error   string
	Saved   bool
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		s.log.Error("read config", "path", s.configPath, "err", err)
		http.Error(w, "cannot read config file", http.StatusInternalServerError)
		return
	}
	s.render(w, pageData{Content: string(data)})
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// CSRF / DNS-rebinding mitigation, not an identity check: reject any
	// mutating request whose Origin/Referer isn't same-origin (absent counts
	// as a mismatch — fail closed).
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	content := r.FormValue("content")

	// Re-run the exact validation the gateway performs at load time; on any
	// error the live file is left untouched.
	if _, err := config.Parse([]byte(content)); err != nil {
		s.render(w, pageData{Content: content, Error: err.Error()})
		return
	}
	if err := s.writeConfig([]byte(content)); err != nil {
		s.log.Error("write config", "path", s.configPath, "err", err)
		s.render(w, pageData{Content: content, Error: "write failed: " + err.Error()})
		return
	}
	s.log.Info("config updated via admin UI", "path", s.configPath)
	s.render(w, pageData{Content: content, Saved: true})
}

// writeConfig writes data to the existing config file in place: O_TRUNC + fsync,
// deliberately NOT temp-file + rename. Atomic rename would require write+execute
// on the parent directory, widening the ACL grant from "this one file" to
// "create/overwrite/delete anything in this directory". The accepted trade-off
// is that a kill at the write syscall can truncate the file; validation runs
// first and the gateway reads config only at startup, so the worst case is
// "gateway refuses to start on a bad file, operator fixes it by hand".
func (s *Server) writeConfig(data []byte) error {
	// No O_CREATE: the file must already exist (installed root:root 0600); we
	// only ever rewrite it, never create, so its mode/owner are preserved.
	f, err := os.OpenFile(s.configPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *Server) render(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		s.log.Error("render page", "err", err)
	}
}

// sameOrigin reports whether a mutating request originates from the same host it
// targets. It prefers Origin and falls back to Referer; if neither is present it
// returns false (fail closed).
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == r.Host
}

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>credential-gateway — config</title>
<style>
  body { font: 14px/1.5 system-ui, sans-serif; max-width: 900px; margin: 2rem auto; padding: 0 1rem; }
  textarea { width: 100%; height: 60vh; font: 13px/1.4 ui-monospace, monospace; box-sizing: border-box; }
  .notice { background: #fff8e1; border: 1px solid #e0c060; padding: .75rem 1rem; border-radius: 4px; }
  .error { background: #fdecea; border: 1px solid #d9534f; padding: .75rem 1rem; border-radius: 4px; white-space: pre-wrap; }
  .saved { background: #e8f5e9; border: 1px solid #4caf50; padding: .75rem 1rem; border-radius: 4px; }
  button { font-size: 14px; padding: .5rem 1.25rem; margin-top: .75rem; }
</style>
</head>
<body>
<h1>credential-gateway config</h1>
<p class="notice"><strong>Restart required.</strong> Saving writes
<code>/etc/credential-gateway/config.yaml</code> but does <strong>not</strong>
affect the running gateway. There is no hot-reload. Apply changes yourself with
<code>sudo systemctl restart credential-gateway</code>.</p>
{{if .Saved}}<p class="saved">Saved. Restart credential-gateway to apply.</p>{{end}}
{{if .Error}}<p class="error"><strong>Rejected — file not changed:</strong>
{{.Error}}</p>{{end}}
<form method="post" action="/save">
  <textarea name="content" spellcheck="false">{{.Content}}</textarea>
  <div><button type="submit">Save</button></div>
</form>
</body>
</html>
`

// ListenAddr formats the loopback bind address for the given port. The host is
// hardcoded to 127.0.0.1: there is deliberately no way to bind a non-loopback
// (e.g. Tailscale) interface — that would be a reachability-model change
// requiring an explicit, separately-justified decision.
func ListenAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}
