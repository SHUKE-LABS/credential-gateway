package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"credential-gateway/internal/config"
)

const validConfig = `http:
  - name: openai
    listen: "127.0.0.1:8080"
    upstream: "https://api.openai.com"
    headers:
      Authorization: "Bearer sk-test"
`

func newTestServer(t *testing.T, initial string) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(path, log), path, dir
}

// post issues a same-origin POST to /save unless overrideOrigin is non-empty
// (empty string means "send no Origin/Referer at all").
func post(s *Server, content, overrideOrigin string, sendHeaders bool) *httptest.ResponseRecorder {
	form := url.Values{"content": {content}}
	req := httptest.NewRequest(http.MethodPost, "/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "127.0.0.1:8099"
	if sendHeaders {
		origin := overrideOrigin
		if origin == "" {
			origin = "http://127.0.0.1:8099"
		}
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestIndexRendersCurrentConfig(t *testing.T) {
	s, _, _ := newTestServer(t, validConfig)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "api.openai.com") {
		t.Errorf("rendered page missing current config content")
	}
	if !strings.Contains(body, "systemctl restart credential-gateway") {
		t.Errorf("rendered page missing restart-required notice")
	}
}

func TestSaveValidWritesInPlace(t *testing.T) {
	s, path, dir := newTestServer(t, validConfig)

	fiBefore, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirBefore, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}

	newContent := validConfig + `redis:
  - listen: "127.0.0.1:6380"
    upstream: "redis:6379"
`
	rec := post(s, newContent, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != newContent {
		t.Errorf("file content not updated")
	}

	// In-place write (O_TRUNC, no rename): the file's own mode is preserved and
	// the parent directory is never modified (no create/rename/delete in it).
	fiAfter, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fiAfter.Mode().Perm() != fiBefore.Mode().Perm() {
		t.Errorf("file mode changed: %o -> %o", fiBefore.Mode().Perm(), fiAfter.Mode().Perm())
	}
	dirAfter, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !dirAfter.ModTime().Equal(dirBefore.ModTime()) {
		t.Errorf("parent dir mtime changed (%v -> %v): a rename/create touched the directory",
			dirBefore.ModTime(), dirAfter.ModTime())
	}
	if dirAfter.Mode().Perm() != dirBefore.Mode().Perm() {
		t.Errorf("parent dir mode changed: %o -> %o", dirBefore.Mode().Perm(), dirAfter.Mode().Perm())
	}
}

func TestSaveInvalidRejectedFileUntouched(t *testing.T) {
	s, path, _ := newTestServer(t, validConfig)

	// Missing required 'upstream' — same rule config.Validate() enforces.
	invalid := `http:
  - name: openai
    listen: "127.0.0.1:8080"
    headers:
      Authorization: "Bearer sk-test"
`
	rec := post(s, invalid, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered with error)", rec.Code)
	}

	// Live file must be unchanged.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != validConfig {
		t.Errorf("invalid submission modified the live file")
	}

	// The surfaced error must match what config.Parse itself produces (per the
	// reviewer's note: assert on the Validate() error, not a path-wrapped one).
	_, wantErr := config.Parse([]byte(invalid))
	if wantErr == nil {
		t.Fatal("test bug: expected invalid config to fail Parse")
	}
	// The page HTML-escapes the error (e.g. ' -> &#39;); compare against the
	// escaped form so we still assert the exact gateway error text is surfaced.
	if !strings.Contains(rec.Body.String(), template.HTMLEscapeString(wantErr.Error())) {
		t.Errorf("page error does not match gateway error text %q", wantErr.Error())
	}
}

func TestSaveOriginChecks(t *testing.T) {
	cases := []struct {
		name        string
		origin      string
		sendHeaders bool
		wantStatus  int
	}{
		{"same origin", "http://127.0.0.1:8099", true, http.StatusOK},
		{"cross origin", "http://evil.example.com", true, http.StatusForbidden},
		{"absent origin/referer", "", false, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, path, _ := newTestServer(t, validConfig)
			rec := post(s, validConfig, tc.origin, tc.sendHeaders)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			// A rejected request must not have written.
			if tc.wantStatus == http.StatusForbidden {
				got, _ := os.ReadFile(path)
				if string(got) != validConfig {
					t.Errorf("rejected cross-origin request still wrote the file")
				}
			}
		})
	}
}

func TestSaveRefererFallback(t *testing.T) {
	s, _, _ := newTestServer(t, validConfig)
	form := url.Values{"content": {validConfig}}
	req := httptest.NewRequest(http.MethodPost, "/save", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "127.0.0.1:8099"
	// No Origin, but a same-origin Referer — should pass.
	req.Header.Set("Referer", "http://127.0.0.1:8099/")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (same-origin Referer fallback)", rec.Code)
	}
}

func TestListenAddrLoopbackOnly(t *testing.T) {
	if got := ListenAddr(8099); got != "127.0.0.1:8099" {
		t.Errorf("ListenAddr(8099) = %q, want 127.0.0.1:8099", got)
	}
}
