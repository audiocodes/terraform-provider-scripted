// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"math/big"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, err := decodeJSON([]byte(s))
	if err != nil {
		t.Fatalf("decodeJSON(%q): %v", s, err)
	}
	return v
}

func num(s string) *big.Float {
	f, _, _ := big.ParseFloat(s, 10, 512, big.ToNearestEven)
	return f
}

func TestJSONToValue_InferredTypes(t *testing.T) {
	t.Parallel()
	got := jsonToValue(mustDecode(t, `{"n": 6, "s": "x", "b": true, "l": [1, "a"], "z": null, "o": {"k": 1.5}}`), nil)
	want := tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"n": tftypes.Number,
		"s": tftypes.String,
		"b": tftypes.Bool,
		"l": tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number, tftypes.String}},
		"z": tftypes.DynamicPseudoType,
		"o": tftypes.Object{AttributeTypes: map[string]tftypes.Type{"k": tftypes.Number}},
	}}, map[string]tftypes.Value{
		"n": tftypes.NewValue(tftypes.Number, num("6")),
		"s": tftypes.NewValue(tftypes.String, "x"),
		"b": tftypes.NewValue(tftypes.Bool, true),
		"l": tftypes.NewValue(tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number, tftypes.String}}, []tftypes.Value{
			tftypes.NewValue(tftypes.Number, num("1")),
			tftypes.NewValue(tftypes.String, "a"),
		}),
		"z": tftypes.NewValue(tftypes.DynamicPseudoType, nil),
		"o": tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{"k": tftypes.Number}}, map[string]tftypes.Value{
			"k": tftypes.NewValue(tftypes.Number, num("1.5")),
		}),
	})
	if !got.Equal(want) {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestJSONToValue_Hints(t *testing.T) {
	t.Parallel()
	listOfNum := tftypes.List{ElementType: tftypes.Number}
	setOfStr := tftypes.Set{ElementType: tftypes.String}
	mapOfStr := tftypes.Map{ElementType: tftypes.String}

	cases := []struct {
		name string
		json string
		hint tftypes.Type
		want tftypes.Type
	}{
		{"list hint kept", `[1, 3]`, listOfNum, listOfNum},
		{"empty list hint kept", `[]`, listOfNum, listOfNum},
		{"list hint with null element kept", `[1, null]`, listOfNum, listOfNum},
		{"list hint dropped on mixed types", `[1, "a"]`, listOfNum, tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number, tftypes.String}}},
		{"set hint kept", `["a", "b"]`, setOfStr, setOfStr},
		{"tuple hint kept", `[1, "a"]`, tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number, tftypes.String}}, tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number, tftypes.String}}},
		{"tuple hint dropped on length change", `[1]`, tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number, tftypes.String}}, tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number}}},
		{"map hint kept", `{"a": "x", "b": "y"}`, mapOfStr, mapOfStr},
		{"map hint dropped on mixed types", `{"a": "x", "b": 1}`, mapOfStr, tftypes.Object{AttributeTypes: map[string]tftypes.Type{"a": tftypes.String, "b": tftypes.Number}}},
		{"null takes hint", `null`, listOfNum, listOfNum},
		{"dynamic hint infers", `[1]`, tftypes.DynamicPseudoType, tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number}}},
		{"nested list hint through object", `{"r": [1, 2]}`,
			tftypes.Object{AttributeTypes: map[string]tftypes.Type{"r": listOfNum}},
			tftypes.Object{AttributeTypes: map[string]tftypes.Type{"r": listOfNum}}},
		{"primitive mismatch is reported, not coerced", `"6"`, tftypes.Number, tftypes.String},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := jsonToValue(mustDecode(t, tc.json), tc.hint)
			if !got.Type().Equal(tc.want) {
				t.Errorf("type: got %s, want %s", got.Type(), tc.want)
			}
			if !got.IsFullyKnown() {
				t.Errorf("value should be known: %s", got)
			}
		})
	}
}

func TestValueToJSON_RoundTripAndUnknowns(t *testing.T) {
	t.Parallel()
	objT := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"n": tftypes.Number,
		"l": tftypes.List{ElementType: tftypes.String},
		"u": tftypes.String,
		"o": tftypes.Object{AttributeTypes: map[string]tftypes.Type{"u2": tftypes.Number}},
	}}
	v := tftypes.NewValue(objT, map[string]tftypes.Value{
		"n": tftypes.NewValue(tftypes.Number, num("6.50")),
		"l": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{tftypes.NewValue(tftypes.String, "a")}),
		"u": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"o": tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{"u2": tftypes.Number}}, map[string]tftypes.Value{
			"u2": tftypes.NewValue(tftypes.Number, tftypes.UnknownValue),
		}),
	})
	got, unknown := valueToJSON(v)
	want := map[string]any{
		"n": json.Number("6.5"),
		"l": []any{"a"},
		"u": nil,
		"o": map[string]any{"u2": nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("json: got %#v, want %#v", got, want)
	}
	if len(unknown) != 2 || !contains(unknown, "u") || !contains(unknown, "o.u2") {
		t.Errorf("unknown paths: got %v", unknown)
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"l":["a"],"n":6.5,"o":{"u2":null},"u":null}` {
		t.Errorf("marshalled: %s", b)
	}
}

func TestJSONEqual(t *testing.T) {
	t.Parallel()
	list := tftypes.NewValue(tftypes.List{ElementType: tftypes.Number}, []tftypes.Value{
		tftypes.NewValue(tftypes.Number, num("1")), tftypes.NewValue(tftypes.Number, num("3")),
	})
	tuple := tftypes.NewValue(tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number, tftypes.Number}}, []tftypes.Value{
		tftypes.NewValue(tftypes.Number, num("1.0")), tftypes.NewValue(tftypes.Number, num("3")),
	})
	other := tftypes.NewValue(tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.Number, tftypes.Number}}, []tftypes.Value{
		tftypes.NewValue(tftypes.Number, num("1")), tftypes.NewValue(tftypes.Number, num("4")),
	})
	unknown := tftypes.NewValue(tftypes.List{ElementType: tftypes.Number}, tftypes.UnknownValue)
	null := tftypes.NewValue(tftypes.DynamicPseudoType, nil)

	if !jsonEqual(list, tuple) {
		t.Error("list and tuple with equal elements should be equal")
	}
	if jsonEqual(list, other) {
		t.Error("different elements should not be equal")
	}
	if jsonEqual(list, unknown) || jsonEqual(unknown, unknown) {
		t.Error("unknown is never equal")
	}
	if !jsonEqual(null, null) {
		t.Error("null equals null")
	}
	if jsonEqual(null, list) {
		t.Error("null is not a list")
	}
}

func TestDecodeJSON_Empty(t *testing.T) {
	t.Parallel()
	v, err := decodeJSON([]byte("  \n"))
	if err != nil || v != nil {
		t.Errorf("empty input: got %v, %v", v, err)
	}
	if _, err := decodeJSON([]byte("{nope")); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
