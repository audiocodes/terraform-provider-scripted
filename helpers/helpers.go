// SPDX-License-Identifier: Apache-2.0

// Package helpers holds the helper libraries the provider ships for programs
// written in particular languages. Only Python has one so far.
package helpers

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed python/scripted.py
var python string

// Python returns the Python helper, scripted.py, with version stamped into
// its HELPER_VERSION.
func Python(version string) string {
	return strings.Replace(python, `HELPER_VERSION = "dev"`, fmt.Sprintf("HELPER_VERSION = %q", version), 1)
}
