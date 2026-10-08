// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"reflect"
	"testing"
)

// FuzzJSONRoundTrip checks that any JSON the program could print survives
// the trip into Terraform's type system and back unchanged, and that the
// conversions never panic on arbitrary input.
func FuzzJSONRoundTrip(f *testing.F) {
	for _, seed := range []string{
		`null`, `true`, `"s"`, `6`, `6.5`, `-0.25`, `123456789012345678901234567890`,
		`[]`, `{}`, `[1, "a", null, true]`, `{"a": {"b": [1, 2]}, "z": null}`,
		`{"n": 1e3}`, `[[], {}, [[]]]`, `{"": ""}`, `"é😀"`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		decoded, err := decodeJSON([]byte(raw))
		if err != nil {
			return // not JSON; nothing to round-trip
		}
		v := jsonToValue(decoded, nil)
		if !v.IsFullyKnown() {
			t.Fatalf("converted value is not fully known: %s", v)
		}
		back, unknown := valueToJSON(v)
		if len(unknown) != 0 {
			t.Fatalf("round trip produced unknowns: %v", unknown)
		}
		// Compare through canonical JSON so 1e3 and 1000 agree.
		want, _ := json.Marshal(decoded)
		got, _ := json.Marshal(back)
		var w, g any
		_ = json.Unmarshal(want, &w)
		_ = json.Unmarshal(got, &g)
		if !reflect.DeepEqual(w, g) {
			t.Fatalf("round trip changed the value:\n in: %s\nout: %s", want, got)
		}
		// Hints must never make the conversion fail or change the value.
		hinted := jsonToValue(decoded, v.Type())
		if !jsonEqual(hinted, v) {
			t.Fatalf("hinting with the value's own type changed it: %s vs %s", hinted, v)
		}
	})
}
