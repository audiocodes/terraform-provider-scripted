// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// doResult says how an operation went from the provider's point of view.
type doResult int

const (
	doOK       doResult = iota
	doDeclined          // not_implemented, and the operation has a default
	doFailed            // a diagnostic has been added; stop
)

// opTitles is how each operation is named in diagnostics.
var opTitles = map[string]string{
	"plan":   "Plan hook",
	"create": "Create",
	"read":   "Read",
	"update": "Update",
	"delete": "Delete",
	"import": "Import",
	"move":   "Move",
	"data":   "Reading data",
}

func opTitle(op string) string {
	if t, ok := opTitles[op]; ok {
		return t
	}
	return strings.ToUpper(op[:1]) + op[1:]
}

// do runs one operation and takes care of what every operation shares:
// turning a run error into a diagnostic, deciding what not_implemented
// means (a default when the operation has one, an error when mandatory),
// reporting the program's warnings and errors, and storing its private
// state. priv may be nil for operations that keep no private state.
func (p program) do(ctx context.Context, rq request, diags *diag.Diagnostics, priv privateWriter, mandatory bool) (*response, doResult) {
	out, err := p.run(ctx, rq)
	if err != nil {
		diags.AddError(opTitle(rq.Op)+" failed", err.Error())
		return nil, doFailed
	}
	if out.notImplemented() {
		if mandatory {
			diags.AddError(notImplementedTitle(rq.Op),
				fmt.Sprintf("The program answered not_implemented to %s, which has no default behaviour.%s", rq.Op, mandatoryHint(rq.Op)))
			return nil, doFailed
		}
		return out, doDeclined
	}
	if out.report(diags, rq.Op) {
		return nil, doFailed
	}
	writePrivate(ctx, priv, out, diags)
	return out, doOK
}

func notImplementedTitle(op string) string {
	if op == "data" {
		return "Data not implemented"
	}
	return opTitle(op) + " not implemented"
}

func mandatoryHint(op string) string {
	if op == "delete" {
		return " To forget a resource without deleting it, use `terraform state rm` or a `removed` block."
	}
	return ""
}
