package toolchain

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/mod/sumdb"
)

// SumGolangOrgKey is the public verifier key for sum.golang.org, as built into
// the go command.
const SumGolangOrgKey = "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"

// NewSumDB returns a Verifier backed by sum.golang.org that caches tiles and
// the latest signed tree head under cacheDir/sumdb.
func NewSumDB(cacheDir string, client *http.Client) Verifier {
	if client == nil {
		client = http.DefaultClient
	}
	ops := &sumdbOps{
		key:    SumGolangOrgKey,
		url:    "https://sum.golang.org",
		dir:    filepath.Join(cacheDir, "sumdb"),
		client: client,
		config: map[string][]byte{},
	}
	return sumdb.NewClient(ops)
}

type sumdbOps struct {
	key    string
	url    string
	dir    string
	client *http.Client

	mu     sync.Mutex
	config map[string][]byte
}

func (o *sumdbOps) ReadRemote(path string) ([]byte, error) {
	resp, err := o.client.Get(o.url + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s%s: %s", o.url, path, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<24))
}

func (o *sumdbOps) ReadConfig(file string) ([]byte, error) {
	if file == "key" {
		return []byte(o.key), nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if data, ok := o.config[file]; ok {
		return data, nil
	}
	data, err := os.ReadFile(o.configPath(file))
	if errors.Is(err, os.ErrNotExist) {
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	o.config[file] = data
	return data, nil
}

func (o *sumdbOps) WriteConfig(file string, old, new []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	cur, ok := o.config[file]
	if !ok {
		cur, _ = os.ReadFile(o.configPath(file))
	}
	if !bytes.Equal(cur, old) {
		return sumdb.ErrWriteConflict
	}
	o.config[file] = new
	// Persisting the tree head is best effort: it only strengthens
	// consistency checks across runs.
	p := o.configPath(file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		tmp := p + ".tmp"
		if os.WriteFile(tmp, new, 0o644) == nil {
			_ = os.Rename(tmp, p)
		}
	}
	return nil
}

func (o *sumdbOps) configPath(file string) string {
	return filepath.Join(o.dir, "config", filepath.FromSlash(safeName(file)))
}

func (o *sumdbOps) ReadCache(file string) ([]byte, error) {
	return os.ReadFile(filepath.Join(o.dir, "cache", filepath.FromSlash(safeName(file))))
}

func (o *sumdbOps) WriteCache(file string, data []byte) {
	p := filepath.Join(o.dir, "cache", filepath.FromSlash(safeName(file)))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tile-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return
	}
	if os.Rename(tmp.Name(), p) != nil {
		os.Remove(tmp.Name())
	}
}

func (o *sumdbOps) Log(string) {}

func (o *sumdbOps) SecurityError(msg string) {
	// The client also returns an error from Lookup, which is what callers act
	// on; nothing else to do here.
}

// safeName keeps sumdb-supplied file names inside our directory.
func safeName(name string) string {
	name = strings.ReplaceAll(name, "..", "_")
	return strings.TrimLeft(name, "/")
}
