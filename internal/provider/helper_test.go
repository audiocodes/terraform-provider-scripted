// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/provider"
)

// resetHelperCache forgets which directory each version's helper went to.
func resetHelperCache(t *testing.T) {
	t.Helper()
	helperMu.Lock()
	helperDirs = map[string]string{}
	helperMu.Unlock()
	t.Cleanup(func() {
		helperMu.Lock()
		helperDirs = map[string]string{}
		helperMu.Unlock()
	})
}

// The helper is written under $TF_DATA_DIR, stamped with the provider
// version, readable by everyone, and not rewritten while it is current.
func TestPythonHelperDir_dataDir(t *testing.T) {
	resetHelperCache(t)
	data := t.TempDir()
	t.Setenv("TF_DATA_DIR", data)
	dir, err := pythonHelperDir(context.Background(), "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(data, "scripted", "python"); dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	path := filepath.Join(dir, "scripted.py")
	got, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(got), `HELPER_VERSION = "9.9.9"`) {
		t.Fatalf("helper not written with its version: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o044 == 0 && os.PathSeparator == '/' {
		t.Errorf("helper should be readable by others, mode %v", fi.Mode())
	}

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	resetHelperCache(t)
	if _, err := pythonHelperDir(context.Background(), "9.9.9"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Error("a current helper must not be rewritten")
	}

	// A different version replaces it.
	resetHelperCache(t)
	if _, err := pythonHelperDir(context.Background(), "9.9.10"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), `HELPER_VERSION = "9.9.10"`) {
		t.Error("an upgrade must replace the helper")
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".scripted-*"))
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

// When the data directory cannot be written, the user cache is used.
func TestPythonHelperDir_fallback(t *testing.T) {
	resetHelperCache(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_DATA_DIR", blocker)
	cache := t.TempDir()
	userCacheDir = func() (string, error) { return cache, nil }
	t.Cleanup(func() { userCacheDir = os.UserCacheDir })

	dir, err := pythonHelperDir(context.Background(), "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cache, "terraform-provider-scripted", "9.9.9", "python"); dir != want {
		t.Fatalf("dir = %q, want the user cache %q", dir, want)
	}

	// And the system temporary directory when there is no user cache.
	resetHelperCache(t)
	userCacheDir = func() (string, error) { return "", errors.New("no cache") }
	dir, err = pythonHelperDir(context.Background(), "9.9.9-tmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(dir)) })
	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Fatalf("dir = %q, want one under %q", dir, os.TempDir())
	}
}

// Loading the schema, which `terraform validate` does, writes the helper.
func TestProviderSchema_writesHelper(t *testing.T) {
	resetHelperCache(t)
	data := t.TempDir()
	t.Setenv("TF_DATA_DIR", data)
	p := New("9.9.8")()
	p.Schema(context.Background(), provider.SchemaRequest{}, &provider.SchemaResponse{})
	if _, err := os.Stat(filepath.Join(data, "scripted", "python", "scripted.py")); err != nil {
		t.Fatalf("schema load should write the helper: %v", err)
	}
}
