package retrieval

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// DocMatchesFilter reports whether doc's metadata satisfies f.
// Doc.Content is not considered except for Match when the key is "_content".
func DocMatchesFilter(d Doc, f Filter) bool {
	if f.Not != nil {
		return !DocMatchesFilter(d, *f.Not)
	}
	for _, sub := range f.And {
		if !DocMatchesFilter(d, sub) {
			return false
		}
	}
	if len(f.Or) > 0 {
		ok := false
		for _, sub := range f.Or {
			if DocMatchesFilter(d, sub) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return matchLeafPredicates(d, f)
}

func matchLeafPredicates(d Doc, f Filter) bool {
	md := d.Metadata
	if len(f.Eq) > 0 {
		for k, want := range f.Eq {
			if !valuesEqual(md[k], want) {
				return false
			}
		}
	}
	if len(f.Neq) > 0 {
		for k, want := range f.Neq {
			if valuesEqual(md[k], want) {
				return false
			}
		}
	}
	if len(f.In) > 0 {
		for k, wantList := range f.In {
			if !sliceContainsAny(md[k], wantList) {
				return false
			}
		}
	}
	if len(f.NotIn) > 0 {
		for k, forbid := range f.NotIn {
			if sliceContainsAny(md[k], forbid) {
				return false
			}
		}
	}
	for k, r := range f.Range {
		if !matchRange(md[k], r) {
			return false
		}
	}
	for _, k := range f.Exists {
		if _, ok := md[k]; !ok {
			return false
		}
	}
	for _, k := range f.Missing {
		if _, ok := md[k]; ok {
			return false
		}
	}
	for k, sub := range f.Match {
		if !strings.Contains(fmt.Sprint(fieldValue(d, k)), sub) {
			return false
		}
	}
	for k, want := range f.Contains {
		if !containsValue(md[k], want, false) {
			return false
		}
	}
	for k, want := range f.IContains {
		if !containsValue(md[k], want, true) {
			return false
		}
	}
	for k, wantList := range f.ContainsAny {
		if !containsAny(md[k], wantList) {
			return false
		}
	}
	for k, wantList := range f.ContainsAll {
		if !containsAll(md[k], wantList) {
			return false
		}
	}
	return true
}

func fieldValue(d Doc, k string) any {
	if k == "_content" {
		return d.Content
	}
	if d.Metadata != nil {
		return d.Metadata[k]
	}
	return nil
}

func valuesEqual(a, b any) bool {
	return reflect.DeepEqual(normalizeJSONish(a), normalizeJSONish(b))
}

func normalizeJSONish(v any) any {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	default:
		return v
	}
}

func sliceContainsAny(v any, list []any) bool {
	for _, want := range list {
		if valuesEqual(v, want) {
			return true
		}
	}
	return false
}

func matchRange(v any, r Range) bool {
	fv, ok := toFloat64(v)
	if !ok {
		return false
	}
	if r.Gt != nil {
		if bound, ok := toFloat64(r.Gt); !ok || !(fv > bound) {
			return false
		}
	}
	if r.Gte != nil {
		if bound, ok := toFloat64(r.Gte); !ok || !(fv >= bound) {
			return false
		}
	}
	if r.Lt != nil {
		if bound, ok := toFloat64(r.Lt); !ok || !(fv < bound) {
			return false
		}
	}
	if r.Lte != nil {
		if bound, ok := toFloat64(r.Lte); !ok || !(fv <= bound) {
			return false
		}
	}
	return true
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	default:
		return 0, false
	}
}

func containsValue(field any, want any, fold bool) bool {
	switch fv := field.(type) {
	case string:
		ws, ok := want.(string)
		if !ok {
			ws = fmt.Sprint(want)
		}
		if fold {
			return strings.Contains(strings.ToLower(fv), strings.ToLower(ws))
		}
		return strings.Contains(fv, ws)
	case []any:
		for _, e := range fv {
			if valuesEqual(e, want) {
				return true
			}
		}
		return false
	case []string:
		ws, _ := want.(string)
		if !fold {
			return slices.Contains(fv, ws)
		}
		for _, e := range fv {
			if strings.EqualFold(e, ws) {
				return true
			}
		}
		return false
	default:
		return valuesEqual(field, want)
	}
}

func containsAny(field any, wantList []any) bool {
	switch fv := field.(type) {
	case []any:
		for _, w := range wantList {
			for _, e := range fv {
				if valuesEqual(e, w) {
					return true
				}
			}
		}
		return false
	case []string:
		for _, w := range wantList {
			ws, _ := w.(string)
			if ws == "" {
				ws = fmt.Sprint(w)
			}
			if slices.Contains(fv, ws) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func containsAll(field any, wantList []any) bool {
	switch fv := field.(type) {
	case []any:
		for _, w := range wantList {
			found := false
			for _, e := range fv {
				if valuesEqual(e, w) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	case []string:
		for _, w := range wantList {
			ws, _ := w.(string)
			if ws == "" {
				ws = fmt.Sprint(w)
			}
			found := slices.Contains(fv, ws)
			if !found {
				return false
			}
		}
		return true
	default:
		return false
	}
}
