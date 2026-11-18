package build

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	good := []string{
		"github.com/org/repo/cmd/handler@0123abc",
		"github.com/org/repo@0123456789abcdef0123456789abcdef01234567",
		"github.com/org/repo/v2/cmd/x@v2.3.4",
		"github.com/org/repo@v0.0.0-20250915201037-7f05d217867b",
		"github.com/org/repo@v1.2.3-rc.1",
	}
	for _, s := range good {
		if _, _, err := ParseTarget(s); err != nil {
			t.Errorf("ParseTarget(%q): %v", s, err)
		}
	}
	bad := []string{
		"github.com/org/repo",
		"github.com/org/repo@latest",
		"github.com/org/repo@main",
		"github.com/org/repo@v1.2",
		"github.com/org/repo@v1.2.3+meta",
		"github.com/org/repo@ABCDEF1",
		"github.com/org/repo@abc",
		"../relative@v1.0.0",
		"@v1.0.0",
	}
	for _, s := range bad {
		if _, _, err := ParseTarget(s); err == nil {
			t.Errorf("ParseTarget(%q) succeeded", s)
		}
	}
}

func TestBinaryName(t *testing.T) {
	cases := []struct{ pkg, mod, want string }{
		{"github.com/org/repo/cmd/handler", "github.com/org/repo", "handler"},
		{"github.com/org/repo", "github.com/org/repo", "repo"},
		{"github.com/org/repo/v2", "github.com/org/repo/v2", "repo"},
		{"github.com/org/repo/v2/cmd/x", "github.com/org/repo/v2", "x"},
	}
	for _, c := range cases {
		if got := BinaryName(c.pkg, c.mod); got != c.want {
			t.Errorf("BinaryName(%q, %q) = %q, want %q", c.pkg, c.mod, got, c.want)
		}
	}
}

func TestDefaultGoPrivate(t *testing.T) {
	if got := DefaultGoPrivate("github.com/acme/lambdas/cmd/x"); got != "github.com/acme" {
		t.Errorf("got %q", got)
	}
	if got := DefaultGoPrivate("gitlab.com/acme/x"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestEnvIsolation(t *testing.T) {
	t.Setenv("GOFLAGS", "-mod=vendor")
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("GIT_CONFIG_COUNT", "5")
	t.Setenv("GOPRIVATE", "corp.example.com")
	env := Env(Options{CacheDir: "/c", GOOS: "linux", GOARCH: "arm64", GoPrivate: []string{"github.com/acme"}, GitHubToken: "tok"})
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := m[k]; dup {
			t.Errorf("duplicate env var %s", k)
		}
		m[k] = v
	}
	want := map[string]string{
		"GOFLAGS":          "-modcacherw",
		"CGO_ENABLED":      "0",
		"GOTOOLCHAIN":      "local",
		"GOENV":            "off",
		"GOWORK":           "off",
		"GOOS":             "linux",
		"GOARCH":           "arm64",
		"GOPRIVATE":        "github.com/acme,corp.example.com",
		"GIT_CONFIG_COUNT": "1",
		"GOMODCACHE":       filepath.Join("/c", "gomodcache"),
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
}

func TestRedactInErrors(t *testing.T) {
	token := "ghp_supersecret"
	dir := t.TempDir()
	// A fake go binary that echoes its environment to stderr and fails.
	goRoot := filepath.Join(dir, "goroot")
	os.MkdirAll(filepath.Join(goRoot, "bin"), 0o755)
	os.WriteFile(filepath.Join(goRoot, "bin", "go"), []byte("#!/bin/sh\nenv >&2\nexit 1\n"), 0o755)
	_, err := Build(context.Background(), Options{
		GoRoot: goRoot, CacheDir: dir, Target: "github.com/acme/x@v1.0.0",
		GOOS: "linux", GOARCH: "arm64", GitHubToken: token,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "x-access-token:***@github.com") {
		t.Fatalf("expected redacted git config in error, got: %v", err)
	}
}

// hostGoRoot finds a local toolchain for network tests.
func hostGoRoot(t *testing.T) string {
	if os.Getenv("GOLAMBDA_NETWORK_TESTS") == "" {
		t.Skip("set GOLAMBDA_NETWORK_TESTS=1 to run")
	}
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Skip("no go on PATH")
	}
	return strings.TrimSpace(string(out))
}

// Two golang/example commits that don't touch hello/.
const (
	helloA = "golang.org/x/example/hello@7f05d217867b"
	helloB = "golang.org/x/example/hello@53021d5c81c3"
)

func TestBuildRealModule(t *testing.T) {
	goRoot := hostGoRoot(t)
	cache := t.TempDir()
	opts := Options{GoRoot: goRoot, CacheDir: cache, Target: helloA, GOOS: "linux", GOARCH: "arm64", Tags: []string{"lambda.norpc"}}

	a, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.BinaryName != "hello" || a.Module != "golang.org/x/example/hello" {
		t.Errorf("result = %+v", a)
	}
	if !strings.HasPrefix(a.ResolvedVersion, "v0.0.0-20250915201037-7f05d217867b") {
		t.Errorf("resolved version = %s", a.ResolvedVersion)
	}
	f, err := elf.NewFile(bytes.NewReader(a.Binary))
	if err != nil {
		t.Fatal(err)
	}
	if f.Machine != elf.EM_AARCH64 {
		t.Errorf("machine = %v", f.Machine)
	}
	bi, err := buildinfo.Read(bytes.NewReader(a.Binary))
	if err != nil {
		t.Fatal(err)
	}
	if bi.Main.Version != "(devel)" {
		t.Errorf("main version embedded: %s", bi.Main.Version)
	}

	// Same inputs, warm cache: identical bytes.
	again, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Binary, again.Binary) {
		t.Error("rebuild differs")
	}

	// A different commit with identical hello/ code: identical bytes.
	opts.Target = helloB
	b, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if b.ResolvedVersion == a.ResolvedVersion {
		t.Error("expected a different resolved version")
	}
	if !bytes.Equal(a.Binary, b.Binary) {
		t.Error("unchanged code at a new commit produced a different binary")
	}

	// Cold cache: identical bytes.
	opts.Target = helloA
	opts.CacheDir = t.TempDir()
	cold, err := Build(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Binary, cold.Binary) {
		t.Error("cold-cache build differs")
	}
}

func TestBuildErrors(t *testing.T) {
	goRoot := hostGoRoot(t)
	cache := t.TempDir()
	for target, want := range map[string]string{
		"golang.org/x/example/hello@0000000000aa":         "resolving",
		"golang.org/x/example/hello/reverse@7f05d217867b": "not a main package",
	} {
		_, err := Build(context.Background(), Options{GoRoot: goRoot, CacheDir: cache, Target: target, GOOS: "linux", GOARCH: "arm64"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want containing %q", target, err, want)
		}
	}
	_, err := Build(context.Background(), Options{GoRoot: goRoot, CacheDir: cache, Target: helloA, GOOS: "linux", GOARCH: "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unsupported GOOS/GOARCH") {
		t.Errorf("bogus arch: err = %v", err)
	}
}
