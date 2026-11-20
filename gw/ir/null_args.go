package ir

// DropNullArgs returns args without null values, the graphql-go v2
// view of a request: a key whose value is nil is removed from args and
// from every input object (map) below it, maps inside lists included.
// A null list item stays. args is returned as is, with no copy, when it
// holds no null. The input is never modified.
//
// Stability: experimental
func DropNullArgs(args map[string]any) map[string]any {
	if !hasNullKey(args) {
		return args
	}
	return dropNullMap(args)
}

func hasNullKey(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			if e == nil || hasNullKey(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasNullKey(e) {
				return true
			}
		}
	}
	return false
}

func dropNullMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, e := range m {
		if e == nil {
			continue
		}
		out[k] = dropNullValue(e)
	}
	return out
}

func dropNullValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if hasNullKey(x) {
			return dropNullMap(x)
		}
	case []any:
		if hasNullKey(x) {
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = dropNullValue(e)
			}
			return out
		}
	}
	return v
}
