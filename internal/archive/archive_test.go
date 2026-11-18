package archive

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

var fakeBinary = bytes.Repeat([]byte("\x7fELF fake binary contents "), 1000)

func TestBuildDeterministic(t *testing.T) {
	for _, args := range [][]string{nil, {"serve", "--flag=it's"}} {
		a, err := Build(fakeBinary, "handler", args)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Build(append([]byte(nil), fakeBinary...), "handler", append([]string(nil), args...))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("args=%q: two builds differ", args)
		}
	}
}

// Golden hashes catch accidental format changes. If you change the archive
// layout on purpose, bump FormatVersion and update these.
func TestBuildGolden(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no-args", nil, "44c6a7fcfecd686e20382f47ebf251137b6e26514f077f195b75cd6a40b9057a"},
		{"args", []string{"serve", "--flag=it's"}, "95adaabf083b73b4d13a27342a31a75a51f4610f123f21438c41c8553fa3c1ca"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			z, err := Build(fakeBinary, "handler", c.args)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(z)
			if got := hex.EncodeToString(sum[:]); got != c.want {
				t.Errorf("sha256 = %s, want %s", got, c.want)
			}
		})
	}
}

func TestBuildLayout(t *testing.T) {
	z, err := Build(fakeBinary, "handler", []string{"serve", "--name=it's here"})
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 2 || zr.File[0].Name != "handler" || zr.File[1].Name != "bootstrap" {
		t.Fatalf("unexpected entries: %v", zr.File)
	}
	for _, f := range zr.File {
		if f.Mode().Perm() != 0o755 {
			t.Errorf("%s mode = %v", f.Name, f.Mode())
		}
		if len(f.Extra) != 0 {
			t.Errorf("%s has extra fields %x", f.Name, f.Extra)
		}
		if y := f.Modified.Year(); y != 1980 {
			t.Errorf("%s modified year = %d", f.Name, y)
		}
	}
	rc, err := zr.File[1].Open()
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(rc)
	want := "#!/bin/sh\nexec \"$LAMBDA_TASK_ROOT/handler\" 'serve' '--name=it'\\''s here'\n"
	if string(script) != want {
		t.Errorf("script =\n%s\nwant\n%s", script, want)
	}
}

func TestBuildBootstrapNameCollision(t *testing.T) {
	z, err := Build(fakeBinary, "bootstrap", []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(z), int64(len(z)))
	if zr.File[0].Name != "bootstrap.bin" {
		t.Fatalf("binary entry = %s", zr.File[0].Name)
	}
}

func TestBuildRejectsNUL(t *testing.T) {
	if _, err := Build(fakeBinary, "handler", []string{"a\x00b"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestWriteFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "out.zip")
	if err := WriteFile(p, []byte("one")); err != nil {
		t.Fatal(err)
	}
	st1, _ := os.Stat(p)
	if err := WriteFile(p, []byte("one")); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(p)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("identical write rewrote the file")
	}
	if err := WriteFile(p, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two" {
		t.Errorf("content = %q", b)
	}
}
