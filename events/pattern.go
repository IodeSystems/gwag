package events

import (
	"fmt"
	"strings"
)

// SegmentKind says how a wildcard segment of a pattern is named.
//
// Stability: experimental
type SegmentKind int

const (
	// SegmentNamed is a "{name}" token: one subject token, named.
	SegmentNamed SegmentKind = iota
	// SegmentPositional is a "*" token: one subject token, named P1, P2, ...
	// in order of the "*" tokens.
	SegmentPositional
	// SegmentRest is a trailing ">" token: one or more subject tokens,
	// named Rest.
	SegmentRest
)

// Segment is one wildcard segment of a pattern.
//
// Stability: experimental
type Segment struct {
	// Name as written for SegmentNamed ("orderId"), "P1".. for
	// SegmentPositional, "Rest" for SegmentRest. Code generators case
	// it for their language.
	Name string
	Kind SegmentKind
}

// Pattern is a parsed channel pattern. The grammar is NATS subjects:
// tokens split on ".", "*" matches one token, ">" (last token only)
// matches the rest. "{name}" matches like "*" and names the token.
//
// Stability: experimental
type Pattern struct {
	raw    string
	tokens []string // literal token, or "" for a wildcard
	segs   []Segment
}

// ParsePattern parses and validates a channel pattern.
//
// Stability: experimental
func ParsePattern(p string) (Pattern, error) {
	if p == "" {
		return Pattern{}, fmt.Errorf("pattern is empty")
	}
	out := Pattern{raw: p}
	parts := strings.Split(p, ".")
	seen := map[string]bool{}
	positional := 0
	for i, t := range parts {
		switch {
		case t == "":
			return Pattern{}, fmt.Errorf("pattern %q: empty token", p)
		case t == "*":
			positional++
			out.segs = append(out.segs, Segment{Name: fmt.Sprintf("P%d", positional), Kind: SegmentPositional})
			out.tokens = append(out.tokens, "")
		case t == ">":
			if i != len(parts)-1 {
				return Pattern{}, fmt.Errorf("pattern %q: \">\" must be the last token", p)
			}
			out.segs = append(out.segs, Segment{Name: "Rest", Kind: SegmentRest})
			out.tokens = append(out.tokens, "")
		case strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}"):
			name := t[1 : len(t)-1]
			if !isIdent(name) {
				return Pattern{}, fmt.Errorf("pattern %q: segment name %q is not an identifier", p, name)
			}
			if seen[name] {
				return Pattern{}, fmt.Errorf("pattern %q: segment %q repeats", p, name)
			}
			seen[name] = true
			out.segs = append(out.segs, Segment{Name: name, Kind: SegmentNamed})
			out.tokens = append(out.tokens, "")
		default:
			if strings.ContainsAny(t, "*>{} \t\r\n") {
				return Pattern{}, fmt.Errorf("pattern %q: bad token %q", p, t)
			}
			out.tokens = append(out.tokens, t)
		}
	}
	return out, nil
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// String returns the pattern as written.
func (p Pattern) String() string { return p.raw }

// Segments returns the wildcard segments in order.
func (p Pattern) Segments() []Segment { return append([]Segment(nil), p.segs...) }

// Wildcard returns the NATS subject filter: "{name}" becomes "*".
func (p Pattern) Wildcard() string {
	out := make([]string, len(p.tokens))
	si := 0
	for i, t := range p.tokens {
		if t != "" {
			out[i] = t
			continue
		}
		if p.segs[si].Kind == SegmentRest {
			out[i] = ">"
		} else {
			out[i] = "*"
		}
		si++
	}
	return strings.Join(out, ".")
}

// Subject fills the wildcard segments, in order, and returns the
// concrete subject.
func (p Pattern) Subject(values []string) (string, error) {
	if len(values) != len(p.segs) {
		return "", fmt.Errorf("pattern %q: %d segment values, want %d", p.raw, len(values), len(p.segs))
	}
	out := make([]string, len(p.tokens))
	si := 0
	for i, t := range p.tokens {
		if t != "" {
			out[i] = t
			continue
		}
		v, seg := values[si], p.segs[si]
		si++
		if seg.Kind == SegmentRest {
			for _, rt := range strings.Split(v, ".") {
				if !validToken(rt) {
					return "", fmt.Errorf("pattern %q: segment %s = %q is not a subject tail", p.raw, seg.Name, v)
				}
			}
		} else if !validToken(v) {
			return "", fmt.Errorf("pattern %q: segment %s = %q is not a subject token", p.raw, seg.Name, v)
		}
		out[i] = v
	}
	return strings.Join(out, "."), nil
}

func validToken(t string) bool {
	return t != "" && !strings.ContainsAny(t, ".*> \t\r\n")
}

// Match reports whether subject matches the pattern and, if so, the
// wildcard segment values in order.
func (p Pattern) Match(subject string) ([]string, bool) {
	parts := strings.Split(subject, ".")
	vals := make([]string, 0, len(p.segs))
	si := 0
	for i, t := range p.tokens {
		if i >= len(parts) {
			return nil, false
		}
		if t != "" {
			if parts[i] != t {
				return nil, false
			}
			continue
		}
		if p.segs[si].Kind == SegmentRest {
			vals = append(vals, strings.Join(parts[i:], "."))
			return vals, true
		}
		vals = append(vals, parts[i])
		si++
	}
	if len(parts) != len(p.tokens) {
		return nil, false
	}
	return vals, true
}
