package toolchain

import (
	"archive/zip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"
)

const testVersion = "1.27.4"

type fakeVerifier map[string]string // version -> h1 hash

func (f fakeVerifier) Lookup(path, version string) ([]string, error) {
	return []string{
		path + " " + version + " " + f[version],
		path + " " + version + "/go.mod h1:ignored=",
	}, nil
}

// fakeToolchainZip writes a minimal toolchain module zip and returns its path
// and h1 hash.
func fakeToolchainZip(t *testing.T, modVersion string) (string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tc.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	prefix := ModulePath + "@" + modVersion + "/"
	for name, body := range map[string]string{
		"bin/go":                       "#!/bin/sh\necho fake go\n",
		"pkg/tool/linux_amd64/compile": "fake",
		"src/runtime/runtime.go":       "package runtime",
		"VERSION":                      "go" + testVersion,
	} {
		w, err := zw.Create(prefix + name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	h, err := dirhash.HashZip(p, dirhash.Hash1)
	if err != nil {
		t.Fatal(err)
	}
	return p, h
}

func fakeProxy(t *testing.T, zipPath string, hits *atomic.Int32) *httptest.Server {
	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/"+ModulePath+"/@v/") || !strings.HasSuffix(r.URL.Path, ".zip") {
			http.NotFound(w, r)
			return
		}
		if !strings.Contains(r.URL.Path, testVersion) {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			hits.Add(1)
		}
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newFetcher(cache, proxy string, v Verifier) *Fetcher {
	return &Fetcher{CacheDir: cache, Proxy: proxy, Verifier: v, GOOS: "linux", GOARCH: "amd64"}
}

func TestEnsureInstallsAndCaches(t *testing.T) {
	mv := ModuleVersion(testVersion, "linux", "amd64")
	zp, h := fakeToolchainZip(t, mv)
	var hits atomic.Int32
	srv := fakeProxy(t, zp, &hits)
	cache := t.TempDir()
	f := newFetcher(cache, srv.URL, fakeVerifier{mv: h})

	root, err := f.Ensure(context.Background(), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	for rel, wantExec := range map[string]bool{
		"bin/go":                       true,
		"pkg/tool/linux_amd64/compile": true,
		"src/runtime/runtime.go":       false,
	} {
		st, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if isExec := st.Mode()&0o111 != 0; isExec != wantExec {
			t.Errorf("%s: mode %v, want exec=%v", rel, st.Mode(), wantExec)
		}
	}
	if _, err := f.Ensure(context.Background(), testVersion); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("downloads = %d, want 1", hits.Load())
	}
	// No leftover temp dirs.
	entries, _ := os.ReadDir(filepath.Join(cache, "toolchains"))
	if len(entries) != 1 {
		t.Errorf("toolchains dir has %d entries", len(entries))
	}
}

func TestEnsureRejectsChecksumMismatch(t *testing.T) {
	mv := ModuleVersion(testVersion, "linux", "amd64")
	zp, _ := fakeToolchainZip(t, mv)
	srv := fakeProxy(t, zp, nil)
	cache := t.TempDir()
	f := newFetcher(cache, srv.URL, fakeVerifier{mv: "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="})
	_, err := f.Ensure(context.Background(), testVersion)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if installed(filepath.Join(cache, "toolchains", mv)) {
		t.Error("mismatched toolchain was installed")
	}
}

func TestEnsureUnknownVersion(t *testing.T) {
	mv := ModuleVersion(testVersion, "linux", "amd64")
	zp, h := fakeToolchainZip(t, mv)
	srv := fakeProxy(t, zp, nil)
	f := newFetcher(t.TempDir(), srv.URL, fakeVerifier{mv: h})
	_, err := f.Ensure(context.Background(), "1.99.0")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.Ensure(context.Background(), "latest"); err == nil {
		t.Fatal("expected invalid version error")
	}
}

// Separate Fetchers simulate separate processes sharing a cache dir.
func TestEnsureConcurrentInstallers(t *testing.T) {
	mv := ModuleVersion(testVersion, "linux", "amd64")
	zp, h := fakeToolchainZip(t, mv)
	srv := fakeProxy(t, zp, nil)
	cache := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = newFetcher(cache, srv.URL, fakeVerifier{mv: h}).Ensure(context.Background(), testVersion)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(cache, "toolchains"))
	if len(entries) != 1 {
		t.Errorf("toolchains dir has %d entries, want 1", len(entries))
	}
}

// TestRealDownload fetches a real toolchain and verifies it against
// sum.golang.org. Opt in with GOLAMBDA_NETWORK_TESTS=1.
func TestRealDownload(t *testing.T) {
	if os.Getenv("GOLAMBDA_NETWORK_TESTS") == "" {
		t.Skip("set GOLAMBDA_NETWORK_TESTS=1 to run")
	}
	cache := t.TempDir()
	f := &Fetcher{CacheDir: cache, Verifier: NewSumDB(cache, nil)}
	root, err := f.Ensure(context.Background(), testVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !installed(root) {
		t.Fatal("go binary missing")
	}
}
