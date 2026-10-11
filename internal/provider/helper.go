// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/audiocodes/terraform-provider-scripted/helpers"
)

// The Python helper is not vendored by programs: the provider writes the copy
// matching its own version to disk and puts its directory on the program's
// PYTHONPATH, so `from scripted import resource` just works. It goes into
// Terraform's data directory (.terraform, or $TF_DATA_DIR), a fixed path
// that editors can be pointed at for completion; it is written as soon as
// Terraform loads the provider's schema, so `terraform validate` after
// `terraform init` creates it.

// pythonHelperFile is the helper's file name; programs import it as `scripted`.
const pythonHelperFile = "scripted.py"

// userCacheDir is os.UserCacheDir, replaceable in tests.
var userCacheDir = os.UserCacheDir

var (
	helperMu   sync.Mutex
	helperDirs = map[string]string{} // version -> directory holding the helper
)

// pythonHelperDir returns the directory holding the Python helper of this
// provider version, writing the file first if it is missing or differs. It
// tries Terraform's data directory, then the user's cache directory, then
// the system temporary directory, so that a read-only working directory
// does not stop programs from running. The result is cached per version.
func pythonHelperDir(ctx context.Context, version string) (string, error) {
	helperMu.Lock()
	defer helperMu.Unlock()
	if dir, ok := helperDirs[version]; ok {
		return dir, nil
	}
	content := []byte(helpers.Python(version))
	var errs []error
	for _, dir := range pythonHelperCandidates(version) {
		if err := writeIfChanged(filepath.Join(dir, pythonHelperFile), content); err != nil {
			errs = append(errs, err)
			tflog.Warn(ctx, "scripted: cannot write the Python helper here, trying the next place", map[string]any{
				"dir": dir, "error": err.Error(),
			})
			continue
		}
		tflog.Debug(ctx, "scripted: Python helper ready", map[string]any{
			"path": filepath.Join(dir, pythonHelperFile), "version": version,
		})
		helperDirs[version] = dir
		return dir, nil
	}
	return "", fmt.Errorf("writing the Python helper: %w", errors.Join(errs...))
}

// pythonHelperCandidates lists where the helper may go, best first, as
// absolute paths (programs run in their own working directories).
func pythonHelperCandidates(version string) []string {
	dataDir := os.Getenv("TF_DATA_DIR")
	if dataDir == "" {
		dataDir = ".terraform"
	}
	var dirs []string
	if abs, err := filepath.Abs(filepath.Join(dataDir, "scripted", "python")); err == nil {
		dirs = append(dirs, abs)
	}
	if cache, err := userCacheDir(); err == nil {
		dirs = append(dirs, filepath.Join(cache, "terraform-provider-scripted", version, "python"))
	}
	return append(dirs, filepath.Join(os.TempDir(), "terraform-provider-scripted-"+version, "python"))
}

// writeIfChanged makes path hold content. Several provider processes may do
// this at once (Terraform starts one per graph walk, and several runs may
// share a directory), so the file is replaced by renaming a complete copy
// over it, never written in place; if the rename loses a race (on Windows
// the target may be open), the file is fine as long as it holds content.
func writeIfChanged(path string, content []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".scripted-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(content)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	// CreateTemp makes the file readable by its owner only.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		if existing, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(existing, content) {
			return nil
		}
		return err
	}
	return nil
}
