package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestLoad_ValidConfig(t *testing.T) {
	path := writeConfig(t, `
http:
  - name: openai
    listen: "127.0.0.1:8080"
    upstream: "https://api.openai.com"
    headers:
      Authorization: "Bearer sk-test"
mysql:
  - listen: "127.0.0.1:3307"
    upstream: "db:3306"
    user: dbuser
    password: secret
redis:
  - listen: "127.0.0.1:6380"
    upstream: "redis:6379"
    password: redissecret
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.HTTP) != 1 || len(cfg.MySQL) != 1 || len(cfg.Redis) != 1 {
		t.Fatalf("wrong listener counts: http=%d mysql=%d redis=%d", len(cfg.HTTP), len(cfg.MySQL), len(cfg.Redis))
	}
}

func TestLoad_MissingRequiredField(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "http missing listen",
			yaml: `
http:
  - upstream: "https://api.example.com"
    headers:
      X-Key: val
`,
			wantErr: "listen",
		},
		{
			name: "http missing upstream",
			yaml: `
http:
  - listen: "127.0.0.1:8080"
    headers:
      X-Key: val
`,
			wantErr: "upstream",
		},
		{
			name: "http missing headers",
			yaml: `
http:
  - listen: "127.0.0.1:8080"
    upstream: "https://api.example.com"
`,
			wantErr: "headers",
		},
		{
			name: "mysql missing user",
			yaml: `
mysql:
  - listen: "127.0.0.1:3307"
    upstream: "db:3306"
    password: secret
`,
			wantErr: "user",
		},
		{
			name: "mysql missing password",
			yaml: `
mysql:
  - listen: "127.0.0.1:3307"
    upstream: "db:3306"
    user: dbuser
`,
			wantErr: "password",
		},
		{
			name: "redis missing upstream",
			yaml: `
redis:
  - listen: "127.0.0.1:6380"
`,
			wantErr: "upstream",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.yaml)
			_, err := Load(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.wantErr != "" {
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err.Error(), tc.wantErr)
				}
			}
		})
	}
}

func TestLoad_UnknownProtocol(t *testing.T) {
	path := writeConfig(t, `
grpc:
  - listen: "127.0.0.1:9090"
    upstream: "backend:9090"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown protocol, got nil")
	}
}

func TestLoad_DuplicateListenAddress(t *testing.T) {
	path := writeConfig(t, `
http:
  - listen: "127.0.0.1:8080"
    upstream: "https://api1.example.com"
    headers:
      X-Key: val
redis:
  - listen: "127.0.0.1:8080"
    upstream: "redis:6379"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate listen address, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error %q does not mention 'duplicate'", err.Error())
	}
}

func TestLoad_WorldReadableConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("redis:\n  - listen: \"127.0.0.1:6380\"\n    upstream: \"redis:6379\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for world-readable config, got nil")
	}
	if !strings.Contains(err.Error(), "permissions") {
		t.Errorf("error %q does not mention 'permissions'", err.Error())
	}
}

func TestLoad_EmptyConfig(t *testing.T) {
	path := writeConfig(t, "{}\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty config, got nil")
	}
	if !strings.Contains(err.Error(), "no listeners") {
		t.Errorf("error %q does not mention 'no listeners'", err.Error())
	}
}

// validACLConfig is a minimal config that passes content validation, so the
// only variable under test in the ACL cases is checkPermissions.
const validACLConfig = `
redis:
  - listen: "127.0.0.1:6380"
    upstream: "redis:6379"
    password: redissecret
`

// setACL applies a POSIX ACL entry to path, skipping the test when setfacl is
// unavailable or the tempdir filesystem rejects ACLs (e.g. a noacl mount) —
// the ACL behaviour cannot be exercised there.
func setACL(t *testing.T, path, spec string) {
	t.Helper()
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl not available")
	}
	out, err := exec.Command("setfacl", "-m", spec, path).CombinedOutput()
	if err != nil {
		t.Skipf("setfacl %q failed (filesystem may lack ACL support): %v: %s", spec, err, out)
	}
}

// TestLoad_ACLNamedUserAccepted covers the bug: a 0600 file with a named-user
// ACL grant reports mode 0660 (the mask reflected into the group bits), which
// the old raw-mode check rejected. The ACL-aware check must accept it.
func TestLoad_ACLNamedUserAccepted(t *testing.T) {
	path := writeConfig(t, validACLConfig)
	setACL(t, path, "u:"+strconv.Itoa(os.Getuid()+1)+":rw")
	if _, err := Load(path); err != nil {
		t.Fatalf("named-user ACL grant should be accepted, got: %v", err)
	}
}

// TestLoad_ACLNamedGroupRejected ensures a named-group entry — group-level
// access — is rejected even though it, too, only shows up as the mask in mode.
func TestLoad_ACLNamedGroupRejected(t *testing.T) {
	path := writeConfig(t, validACLConfig)
	setACL(t, path, "g:"+strconv.Itoa(os.Getgid()+1)+":r")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for named-group ACL entry, got nil")
	}
	if !strings.Contains(err.Error(), "group") {
		t.Errorf("error %q does not mention the group access", err.Error())
	}
}

// TestLoad_ACLOwningGroupRejected ensures real group:: access is rejected even
// when an extended ACL is present. A named-user entry forces the xattr to exist
// (so the ACL-aware path, not the mode fallback, evaluates the file), and the
// owning-group entry carries read — which must be rejected, not masked away.
func TestLoad_ACLOwningGroupRejected(t *testing.T) {
	path := writeConfig(t, validACLConfig)
	setACL(t, path, "u:"+strconv.Itoa(os.Getuid()+1)+":rw,g::r")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for group:: ACL access, got nil")
	}
	if !strings.Contains(err.Error(), "group") {
		t.Errorf("error %q does not mention the group access", err.Error())
	}
}

// TestLoad_NoACLGroupReadableRejected is the no-xattr fallback: a plain 0640
// file (no extended ACL) must still be rejected by the raw mode check.
func TestLoad_NoACLGroupReadableRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(validACLConfig), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for group-readable no-ACL config, got nil")
	}
	if !strings.Contains(err.Error(), "permissions") {
		t.Errorf("error %q does not mention 'permissions'", err.Error())
	}
}
