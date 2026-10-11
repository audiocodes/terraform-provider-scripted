// SPDX-License-Identifier: Apache-2.0

package helpers

import (
	"strings"
	"testing"
)

func TestPython_stampsVersion(t *testing.T) {
	got := Python("1.2.3")
	if !strings.Contains(got, `HELPER_VERSION = "1.2.3"`) || strings.Contains(got, `HELPER_VERSION = "dev"`) {
		t.Fatal("the version is not stamped into HELPER_VERSION")
	}
	if !strings.Contains(Python("dev"), `HELPER_VERSION = "dev"`) {
		t.Fatal("dev stays dev")
	}
}
