// Package archive writes byte-for-byte reproducible Lambda zip archives.
//
// The output depends only on the inputs and on the Go version used to compile
// this provider (compress/flate output is not guaranteed stable across Go
// releases).
package archive

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FormatVersion changes whenever the archive layout changes in a way that
// alters output bytes for the same inputs.
const FormatVersion = 1

// BootstrapName is the executable the Lambda custom runtime invokes.
const BootstrapName = "bootstrap"

// zip stores MS-DOS timestamps; 1980-01-01 00:00:00 is the earliest value.
const (
	dosDate1980 = 1<<5 | 1 // year 0 (1980), month 1, day 1
	dosTime0    = 0
)

// Build returns the zip bytes for a Lambda package.
//
// If args is empty the archive holds a single "bootstrap" entry containing
// binary. Otherwise the binary is stored under binaryName and "bootstrap" is a
// POSIX shell script that execs it with args.
func Build(binary []byte, binaryName string, args []string) ([]byte, error) {
	type entry struct {
		name string
		data []byte
	}
	var entries []entry
	if len(args) == 0 {
		entries = []entry{{BootstrapName, binary}}
	} else {
		name, err := scriptTargetName(binaryName)
		if err != nil {
			return nil, err
		}
		script, err := Script(name, args)
		if err != nil {
			return nil, err
		}
		entries = []entry{{name, binary}, {BootstrapName, script}}
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(w, flate.BestCompression)
	})
	for _, e := range entries {
		fh := &zip.FileHeader{
			Name:   e.name,
			Method: zip.Deflate,
			// Leave Modified zero: setting it makes archive/zip emit an
			// extended-timestamp extra field. Use the legacy DOS fields.
			ModifiedDate: dosDate1980,
			ModifiedTime: dosTime0,
		}
		fh.SetMode(0o755) // also marks the creator as Unix
		w, err := zw.CreateHeader(fh)
		if err != nil {
			return nil, fmt.Errorf("adding %s to zip: %w", e.name, err)
		}
		if _, err := w.Write(e.data); err != nil {
			return nil, fmt.Errorf("writing %s to zip: %w", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("finishing zip: %w", err)
	}
	return buf.Bytes(), nil
}

// scriptTargetName picks the in-archive name for the binary when a wrapper
// script occupies "bootstrap".
func scriptTargetName(binaryName string) (string, error) {
	if binaryName == "" || strings.ContainsAny(binaryName, "/\\\x00") || binaryName == "." || binaryName == ".." {
		return "", fmt.Errorf("invalid binary name %q", binaryName)
	}
	if binaryName == BootstrapName {
		return BootstrapName + ".bin", nil
	}
	return binaryName, nil
}

// Script renders the bootstrap wrapper that execs binaryName with args.
func Script(binaryName string, args []string) ([]byte, error) {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`exec "$LAMBDA_TASK_ROOT/` + binaryName + `"`)
	for _, a := range args {
		if strings.ContainsRune(a, 0) {
			return nil, errors.New("args must not contain NUL bytes")
		}
		b.WriteByte(' ')
		b.WriteString(ShellQuote(a))
	}
	b.WriteByte('\n')
	return []byte(b.String()), nil
}

// ShellQuote returns s as a single POSIX shell word.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteFile atomically writes data to path, creating parent directories. If
// path already holds exactly data, it is left untouched.
func WriteFile(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".golambda-*.zip.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
