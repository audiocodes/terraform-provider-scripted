// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strconv"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// decodeJSON parses raw JSON into the generic shapes jsonToValue expects:
// map[string]any, []any, json.Number, string, bool and nil. Numbers stay as
// json.Number so large integers survive untouched.
func decodeJSON(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// jsonToValue converts decoded JSON into a tftypes.Value.
//
// hint is the type the result should conform to where the JSON shape allows
// it, typically the type the attribute already has in state so that a list
// stays a list rather than turning into a tuple on every refresh. Where the
// JSON does not fit the hint the inferred type is used instead; the mismatch
// then surfaces as an ordinary plan diff rather than a provider error.
func jsonToValue(v any, hint tftypes.Type) tftypes.Value {
	if hint != nil && hint.Is(tftypes.DynamicPseudoType) {
		hint = nil
	}
	switch x := v.(type) {
	case nil:
		if hint != nil {
			return tftypes.NewValue(hint, nil)
		}
		return tftypes.NewValue(tftypes.DynamicPseudoType, nil)
	case bool:
		return tftypes.NewValue(tftypes.Bool, x)
	case string:
		return tftypes.NewValue(tftypes.String, x)
	case json.Number:
		f, _, err := big.ParseFloat(x.String(), 10, 512, big.ToNearestEven)
		if err != nil {
			// Not a number after all; keep it visible rather than dropping it.
			return tftypes.NewValue(tftypes.String, x.String())
		}
		return tftypes.NewValue(tftypes.Number, f)
	case float64:
		return tftypes.NewValue(tftypes.Number, big.NewFloat(x))
	case map[string]any:
		return jsonObjectToValue(x, hint)
	case []any:
		return jsonArrayToValue(x, hint)
	default:
		return tftypes.NewValue(tftypes.String, fmt.Sprint(x))
	}
}

func jsonObjectToValue(m map[string]any, hint tftypes.Type) tftypes.Value {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// A map hint is honoured only if every element ends up with its element
	// type; otherwise the result is an object, which can hold mixed types.
	if mt, ok := hint.(tftypes.Map); ok {
		elems := make(map[string]tftypes.Value, len(m))
		uniform := true
		for _, k := range keys {
			ev := jsonToValue(m[k], mt.ElementType)
			if !ev.Type().Equal(mt.ElementType) && !ev.IsNull() {
				uniform = false
				break
			}
			elems[k] = tftypes.NewValue(mt.ElementType, valueContent(ev))
		}
		if uniform {
			return tftypes.NewValue(mt, elems)
		}
	}

	var attrHints map[string]tftypes.Type
	if ot, ok := hint.(tftypes.Object); ok {
		attrHints = ot.AttributeTypes
	}

	attrTypes := make(map[string]tftypes.Type, len(m))
	attrs := make(map[string]tftypes.Value, len(m))
	for _, k := range keys {
		var h tftypes.Type
		if attrHints != nil {
			h = attrHints[k]
		}
		ev := jsonToValue(m[k], h)
		attrTypes[k] = ev.Type()
		attrs[k] = ev
	}
	return tftypes.NewValue(tftypes.Object{AttributeTypes: attrTypes}, attrs)
}

func jsonArrayToValue(a []any, hint tftypes.Type) tftypes.Value {
	switch ht := hint.(type) {
	case tftypes.List:
		if elems, ok := uniformElements(a, ht.ElementType); ok {
			return tftypes.NewValue(ht, elems)
		}
	case tftypes.Set:
		if elems, ok := uniformElements(a, ht.ElementType); ok {
			return tftypes.NewValue(ht, elems)
		}
	case tftypes.Tuple:
		if len(ht.ElementTypes) == len(a) {
			elems := make([]tftypes.Value, len(a))
			types := make([]tftypes.Type, len(a))
			for i, e := range a {
				elems[i] = jsonToValue(e, ht.ElementTypes[i])
				types[i] = elems[i].Type()
			}
			return tftypes.NewValue(tftypes.Tuple{ElementTypes: types}, elems)
		}
	}

	elems := make([]tftypes.Value, len(a))
	types := make([]tftypes.Type, len(a))
	for i, e := range a {
		elems[i] = jsonToValue(e, nil)
		types[i] = elems[i].Type()
	}
	return tftypes.NewValue(tftypes.Tuple{ElementTypes: types}, elems)
}

// uniformElements converts every element with elemType as the hint and
// reports whether they all came out as that type (nulls count as matching).
func uniformElements(a []any, elemType tftypes.Type) ([]tftypes.Value, bool) {
	elems := make([]tftypes.Value, len(a))
	for i, e := range a {
		ev := jsonToValue(e, elemType)
		if !ev.Type().Equal(elemType) && !ev.IsNull() {
			return nil, false
		}
		elems[i] = tftypes.NewValue(elemType, valueContent(ev))
	}
	return elems, true
}

// valueContent returns the Go value wrapped by v so it can be re-wrapped with
// a different (compatible) type, as happens when a null takes a collection's
// element type.
func valueContent(v tftypes.Value) any {
	if v.IsNull() {
		return nil
	}
	var out any
	switch {
	case v.Type().Is(tftypes.String):
		var s string
		_ = v.As(&s)
		out = s
	case v.Type().Is(tftypes.Number):
		var f *big.Float
		_ = v.As(&f)
		out = f
	case v.Type().Is(tftypes.Bool):
		var b bool
		_ = v.As(&b)
		out = b
	case v.Type().Is(tftypes.Object{}) || v.Type().Is(tftypes.Map{}):
		var m map[string]tftypes.Value
		_ = v.As(&m)
		out = m
	default:
		var l []tftypes.Value
		_ = v.As(&l)
		out = l
	}
	return out
}

// valueToJSON converts a tftypes.Value into the generic JSON shapes that
// encoding/json marshals. Unknown values become null and their paths are
// collected so the script can be told which parts are not yet decided.
func valueToJSON(v tftypes.Value) (any, []string) {
	var unknown []string
	out := valueToJSONAt(v, "", &unknown)
	return out, unknown
}

func valueToJSONAt(v tftypes.Value, p string, unknown *[]string) any {
	if !v.IsKnown() {
		*unknown = append(*unknown, pathOrRoot(p))
		return nil
	}
	if v.IsNull() {
		return nil
	}
	t := v.Type()
	switch {
	case t.Is(tftypes.String):
		var s string
		_ = v.As(&s)
		return s
	case t.Is(tftypes.Number):
		var f *big.Float
		_ = v.As(&f)
		return json.Number(f.Text('f', -1))
	case t.Is(tftypes.Bool):
		var b bool
		_ = v.As(&b)
		return b
	case t.Is(tftypes.Object{}) || t.Is(tftypes.Map{}):
		var m map[string]tftypes.Value
		_ = v.As(&m)
		out := make(map[string]any, len(m))
		for k, ev := range m {
			out[k] = valueToJSONAt(ev, joinPath(p, k), unknown)
		}
		return out
	default: // list, set, tuple
		var l []tftypes.Value
		_ = v.As(&l)
		out := make([]any, len(l))
		for i, ev := range l {
			out[i] = valueToJSONAt(ev, p+"["+strconv.Itoa(i)+"]", unknown)
		}
		return out
	}
}

func joinPath(p, k string) string {
	if p == "" {
		return k
	}
	return p + "." + k
}

func pathOrRoot(p string) string {
	if p == "" {
		return "."
	}
	return p
}

// jsonEqual reports whether two values are the same once reduced to JSON,
// so that a list and a tuple with the same elements, or 6 and 6.0, compare
// equal. Anything unknown is never equal to anything.
func jsonEqual(a, b tftypes.Value) bool {
	ja, ua := valueToJSON(a)
	jb, ub := valueToJSON(b)
	if len(ua) > 0 || len(ub) > 0 {
		return false
	}
	return reflect.DeepEqual(ja, jb)
}
