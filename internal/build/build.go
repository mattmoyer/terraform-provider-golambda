// Package build compiles a main package from a remote Go module into a
// reproducible static binary.
//
// It does not use `go install pkg@version`: that embeds the module version in
// the binary's build info, so two commits with identical code would produce
// different bytes. Instead it resolves the module containing the package,
// then builds from inside that module's (read-only) module cache directory as
// the main module, which records the version as "(devel)".
package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// Options describes a single build.
type Options struct {
	GoRoot      string   // toolchain root; GoRoot/bin/go is used
	CacheDir    string   // holds GOMODCACHE, GOCACHE and GOPATH
	Target      string   // "<package>@<version>"
	GOOS        string   // target OS
	GOARCH      string   // target architecture
	Tags        []string // build tags
	LDFlags     string   // extra linker flags, appended to the fixed ones
	GitHubToken string   // optional; used for private github.com repos
	GoPrivate   []string // GOPRIVATE patterns
}

// Result is a built binary.
type Result struct {
	Binary          []byte
	BinaryName      string // what `go install` would name it
	Module          string // module path containing the package
	ResolvedVersion string // canonical (pseudo-)version the target resolved to
}

// FixedLDFlags are always passed to the linker, before Options.LDFlags.
const FixedLDFlags = "-s -w -buildid="

var shaRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ParseTarget splits "<package>@<version>" and validates both halves. The
// version must be a commit hash or an exact semantic version; queries such
// as "latest" or branch names are rejected because they aren't stable.
func ParseTarget(target string) (pkg, version string, err error) {
	i := strings.LastIndex(target, "@")
	if i < 0 {
		return "", "", fmt.Errorf("target %q must be <package>@<version>", target)
	}
	pkg, version = target[:i], target[i+1:]
	if err := module.CheckImportPath(pkg); err != nil {
		return "", "", fmt.Errorf("target %q: %w", target, err)
	}
	if shaRE.MatchString(version) {
		return pkg, version, nil
	}
	if semver.IsValid(version) && semver.Canonical(version) == version {
		return pkg, version, nil
	}
	return "", "", fmt.Errorf("target %q: version %q must be a commit hash (7-40 lowercase hex characters) or an exact semantic version like v1.2.3", target, version)
}

// DefaultGoPrivate returns the GOPRIVATE pattern for a package's repository
// owner on github.com ("github.com/<owner>"), or "" for other hosts.
func DefaultGoPrivate(pkg string) string {
	parts := strings.Split(pkg, "/")
	if len(parts) >= 2 && parts[0] == "github.com" {
		return "github.com/" + parts[1]
	}
	return ""
}

// inheritedEnv lists the variables passed through from the provider's
// environment. Everything else that could change the build (GOFLAGS, go.env,
// go.work, CGO settings...) is deliberately not inherited.
var inheritedEnv = []string{
	"PATH", "HOME", "USER", "TMPDIR", "SYSTEMROOT", "LOCALAPPDATA",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	// Module download configuration may legitimately differ per site.
	"GOPROXY", "GOSUMDB", "GONOPROXY", "GONOSUMDB", "GOINSECURE", "GOAUTH",
}

// Env returns the environment for go commands run with o.
func Env(o Options) []string {
	var env []string
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	private := append([]string(nil), o.GoPrivate...)
	if v := os.Getenv("GOPRIVATE"); v != "" {
		private = append(private, v)
	}
	env = append(env,
		"GOENV=off",
		"GOFLAGS=-modcacherw", // lets the cache dir be deleted normally
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GO111MODULE=on",
		"CGO_ENABLED=0",
		"GOOS="+o.GOOS,
		"GOARCH="+o.GOARCH,
		"GOPATH="+filepath.Join(o.CacheDir, "gopath"),
		"GOMODCACHE="+filepath.Join(o.CacheDir, "gomodcache"),
		"GOCACHE="+filepath.Join(o.CacheDir, "gocache"),
		"GOPRIVATE="+strings.Join(private, ","),
		"GIT_TERMINAL_PROMPT=0",
	)
	if _, ok := os.LookupEnv("GOPROXY"); !ok {
		env = append(env, "GOPROXY=https://proxy.golang.org,direct")
	}
	if o.GitHubToken != "" {
		// Rewrite GitHub URLs to carry the token, via env-only git config so
		// nothing is written to disk.
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=url.https://x-access-token:"+o.GitHubToken+"@github.com/.insteadOf",
			"GIT_CONFIG_VALUE_0=https://github.com/",
		)
	}
	return env
}

// Build resolves and compiles o.Target.
func Build(ctx context.Context, o Options) (*Result, error) {
	pkg, version, err := ParseTarget(o.Target)
	if err != nil {
		return nil, err
	}
	if o.GOOS == "" || o.GOARCH == "" {
		return nil, errors.New("GOOS and GOARCH are required")
	}
	r := &runner{goBin: filepath.Join(o.GoRoot, "bin", "go"), env: Env(o), token: o.GitHubToken}

	work, err := os.MkdirTemp("", "golambda-build-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	// Resolve the module that provides pkg at version using a throwaway main
	// module. This is the same resolution `go install pkg@version` does.
	resolver := filepath.Join(work, "resolve")
	if err := os.Mkdir(resolver, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(resolver, "go.mod"), []byte("module golambdaresolve\n"), 0o644); err != nil {
		return nil, err
	}
	if _, err := r.run(ctx, resolver, "get", pkg+"@"+version); err != nil {
		return nil, fmt.Errorf("resolving %s: %w", o.Target, err)
	}
	out, err := r.run(ctx, resolver, "list", "-json=Name,Module", pkg)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", o.Target, err)
	}
	var info struct {
		Name   string
		Module *struct{ Path, Version, Dir string }
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parsing go list output: %w", err)
	}
	if info.Module == nil || info.Module.Dir == "" {
		return nil, fmt.Errorf("%s: could not determine its module", pkg)
	}
	if info.Name != "main" {
		return nil, fmt.Errorf("%s is package %s, not a main package", pkg, info.Name)
	}

	rel := "."
	if pkg != info.Module.Path {
		rel = "./" + strings.TrimPrefix(pkg, info.Module.Path+"/")
	}
	name := BinaryName(pkg, info.Module.Path)
	binPath := filepath.Join(work, "out", name)

	args := []string{"build", "-trimpath", "-buildvcs=false", "-mod=readonly"}
	if len(o.Tags) > 0 {
		args = append(args, "-tags="+strings.Join(o.Tags, ","))
	}
	ldflags := FixedLDFlags
	if strings.TrimSpace(o.LDFlags) != "" {
		ldflags += " " + strings.TrimSpace(o.LDFlags)
	}
	args = append(args, "-ldflags="+ldflags, "-o", binPath, rel)
	if _, err := r.run(ctx, info.Module.Dir, args...); err != nil {
		return nil, fmt.Errorf("building %s: %w", o.Target, err)
	}
	bin, err := os.ReadFile(binPath)
	if err != nil {
		return nil, err
	}
	return &Result{
		Binary:          bin,
		BinaryName:      name,
		Module:          info.Module.Path,
		ResolvedVersion: info.Module.Version,
	}, nil
}

// BinaryName mirrors the go command: the last import path element, skipping a
// trailing major-version suffix of the module path.
func BinaryName(pkg, modPath string) string {
	base := path.Base(pkg)
	if pkg == modPath {
		if prefix, major, ok := module.SplitPathVersion(modPath); ok && strings.HasPrefix(major, "/") {
			return path.Base(prefix)
		}
	}
	return base
}

type runner struct {
	goBin string
	env   []string
	token string
}

func (r *runner) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.goBin, args...)
	cmd.Dir = dir
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(Redact(msg, r.token))
	}
	return stdout.Bytes(), nil
}

// Redact removes secret from s.
func Redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}
