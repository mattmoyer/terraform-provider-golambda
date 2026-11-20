// Package toolchain downloads and installs Go toolchains from the Go module
// proxy, the same artifacts the go command uses for GOTOOLCHAIN switching.
package toolchain

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/mod/sumdb/dirhash"
)

// ModulePath is the module under which Go toolchains are published.
const ModulePath = "golang.org/toolchain"

// DefaultProxy is the Go module proxy toolchains are fetched from.
const DefaultProxy = "https://proxy.golang.org"

var versionRE = regexp.MustCompile(`^1\.\d+(\.\d+)?((rc|beta)\d+)?$`)

// ValidVersion reports whether v looks like a Go release version such as
// "1.27.4" or "1.28rc1" (no "go" prefix).
func ValidVersion(v string) bool { return versionRE.MatchString(v) }

// ModuleVersion returns the toolchain module version for a Go version and
// host platform, e.g. "v0.0.1-go1.27.4.linux-amd64".
func ModuleVersion(goVersion, goos, goarch string) string {
	return "v0.0.1-go" + goVersion + "." + goos + "-" + goarch
}

// Verifier returns the go.sum lines for a module version from a trusted
// checksum database.
type Verifier interface {
	Lookup(path, version string) ([]string, error)
}

// Fetcher installs toolchains into CacheDir/toolchains.
type Fetcher struct {
	CacheDir   string
	Proxy      string // defaults to DefaultProxy
	Verifier   Verifier
	HTTPClient *http.Client // defaults to http.DefaultClient
	GOOS       string       // host platform; defaults to runtime.GOOS
	GOARCH     string       // host platform; defaults to runtime.GOARCH

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Ensure returns the GOROOT of an installed toolchain for goVersion,
// downloading and verifying it first if needed.
func (f *Fetcher) Ensure(ctx context.Context, goVersion string) (string, error) {
	if !ValidVersion(goVersion) {
		return "", fmt.Errorf("invalid Go version %q", goVersion)
	}
	goos, goarch := f.GOOS, f.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	modVersion := ModuleVersion(goVersion, goos, goarch)
	root := filepath.Join(f.CacheDir, "toolchains", modVersion)

	// Serialize installs of the same version within this process so parallel
	// resources don't all download the same ~100MB file. Other processes are
	// handled by the atomic rename below.
	lock := f.lockFor(modVersion)
	lock.Lock()
	defer lock.Unlock()

	if installed(root) {
		return root, nil
	}
	if err := f.install(ctx, modVersion, root); err != nil {
		return "", fmt.Errorf("installing Go %s for %s/%s: %w", goVersion, goos, goarch, err)
	}
	return root, nil
}

func (f *Fetcher) lockFor(key string) *sync.Mutex {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.locks == nil {
		f.locks = map[string]*sync.Mutex{}
	}
	if f.locks[key] == nil {
		f.locks[key] = &sync.Mutex{}
	}
	return f.locks[key]
}

func installed(root string) bool {
	st, err := os.Stat(filepath.Join(root, "bin", goExe()))
	return err == nil && st.Mode().IsRegular()
}

func goExe() string {
	if runtime.GOOS == "windows" {
		return "go.exe"
	}
	return "go"
}

// GoBinary returns the path of the go command inside root.
func GoBinary(root string) string { return filepath.Join(root, "bin", goExe()) }

func (f *Fetcher) install(ctx context.Context, modVersion, root string) error {
	if f.Verifier == nil {
		return errors.New("no checksum verifier configured")
	}
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp(parent, ".install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	zipPath := filepath.Join(work, "toolchain.zip")
	if err := f.download(ctx, modVersion, zipPath); err != nil {
		return err
	}
	if err := f.verify(modVersion, zipPath); err != nil {
		return err
	}
	extracted := filepath.Join(work, "goroot")
	if err := extract(zipPath, ModulePath+"@"+modVersion+"/", extracted); err != nil {
		return err
	}
	if !installed(extracted) {
		return errors.New("toolchain archive has no bin/go")
	}
	if err := os.Rename(extracted, root); err != nil {
		// Another process may have won the race; that's fine if its copy is
		// complete.
		if installed(root) {
			return nil
		}
		return err
	}
	return nil
}

func (f *Fetcher) download(ctx context.Context, modVersion, dest string) error {
	proxy := strings.TrimRight(f.Proxy, "/")
	if proxy == "" {
		proxy = DefaultProxy
	}
	url := proxy + "/" + ModulePath + "/@v/" + modVersion + ".zip"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := f.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return fmt.Errorf("toolchain %s not found on %s (is the Go version correct?)", modVersion, proxy)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	return out.Close()
}

func (f *Fetcher) verify(modVersion, zipPath string) error {
	got, err := dirhash.HashZip(zipPath, dirhash.Hash1)
	if err != nil {
		return fmt.Errorf("hashing toolchain zip: %w", err)
	}
	lines, err := f.Verifier.Lookup(ModulePath, modVersion)
	if err != nil {
		return fmt.Errorf("checksum database lookup: %w", err)
	}
	prefix := ModulePath + " " + modVersion + " "
	for _, line := range lines {
		if want, ok := strings.CutPrefix(line, prefix); ok {
			if strings.TrimSpace(want) == got {
				return nil
			}
			return fmt.Errorf("checksum mismatch for %s: downloaded %s, checksum database has %s", modVersion, got, want)
		}
	}
	return fmt.Errorf("checksum database has no entry for %s@%s", ModulePath, modVersion)
}

// extract unpacks entries under prefix into dest. Module zips don't record
// file modes, so, like the go command, it marks bin/ and pkg/tool/ executable.
func extract(zipPath, prefix, dest string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		rel, ok := strings.CutPrefix(zf.Name, prefix)
		if !ok {
			return fmt.Errorf("unexpected file %q in toolchain zip", zf.Name)
		}
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("unsafe path %q in toolchain zip", zf.Name)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(rel, "bin/") || strings.HasPrefix(rel, "pkg/tool/") {
			mode = 0o755
		}
		if err := extractFile(zf, filepath.Join(dest, filepath.FromSlash(rel)), mode); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(zf *zip.File, path string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
