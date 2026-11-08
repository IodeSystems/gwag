package eventgen

import (
	"strings"
)

// This file ports betterproto2_compiler's casing.py so generated event
// code names the classes and fields the same as betterproto2's output.
// python_test.go holds a table captured from the pinned compiler.

func isUpperASCII(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLowerASCII(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigitASCII(c byte) bool { return c >= '0' && c <= '9' }
func isAlnumASCII(c byte) bool { return isUpperASCII(c) || isLowerASCII(c) || isDigitASCII(c) }

// pyPascalCase is casing.pascal_case(value) (strict): split into words
// the way the compiler's regexp does, capitalize each, drop the rest.
func pyPascalCase(s string) string {
	var out strings.Builder
	i, n := 0, len(s)
	for i < n {
		for i < n && !isAlnumASCII(s[i]) {
			i++
		}
		if i >= n {
			break
		}
		j := pyWordEnd(s, i)
		if j == i { // cannot happen for an alnum start; avoid looping
			j = i + 1
		}
		out.WriteString(pyCapitalize(s[i:j]))
		i = j
	}
	return out.String()
}

// pyWordEnd returns the end of the word starting at i:
// WORD_UPPER = [A-Z]+(?![a-z])[0-9]* tried first, then
// WORD = [A-Z]*[a-z]*[0-9]*.
func pyWordEnd(s string, i int) int {
	n := len(s)
	j := i
	for j < n && isUpperASCII(s[j]) {
		j++
	}
	// [A-Z]+ backtracks until what follows is not a lowercase letter.
	for k := j; k > i; k-- {
		if k < n && isLowerASCII(s[k]) {
			continue
		}
		for k < n && isDigitASCII(s[k]) {
			k++
		}
		return k
	}
	// WORD: [A-Z]* is empty here or followed by lowercase.
	k := i
	for k < n && isUpperASCII(s[k]) {
		k++
	}
	for k < n && isLowerASCII(s[k]) {
		k++
	}
	for k < n && isDigitASCII(s[k]) {
		k++
	}
	return k
}

// pyCapitalize is str.capitalize for ASCII.
func pyCapitalize(w string) string {
	if w == "" {
		return w
	}
	return strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
}

// pySnakeCase is casing.snake_case.
func pySnakeCase(name string) string {
	hasUpper := false
	for i := 0; i < len(name); i++ {
		if isUpperASCII(name[i]) {
			hasUpper = true
		}
	}
	if strings.Contains(name, "_") || !hasUpper {
		return name
	}
	// Each pass reads neighbours from its own input, like re.sub's
	// lookbehind and lookahead.
	pass := func(in string, ins func(prev, cur byte, next int) bool) string {
		var b strings.Builder
		for i := 0; i < len(in); i++ {
			var prev byte
			if i > 0 {
				prev = in[i-1]
			}
			next := -1
			if i+1 < len(in) {
				next = int(in[i+1])
			}
			if ins(prev, in[i], next) {
				b.WriteByte('_')
			}
			b.WriteByte(in[i])
		}
		return b.String()
	}
	name = pass(name, func(prev, cur byte, _ int) bool {
		return isUpperASCII(cur) && (isLowerASCII(prev) || isDigitASCII(prev))
	})
	name = pass(name, func(prev, cur byte, next int) bool {
		return isUpperASCII(cur) && isUpperASCII(prev) && next >= 0 && isLowerASCII(byte(next))
	})
	name = pass(name, func(prev, cur byte, _ int) bool {
		return isDigitASCII(cur) && (isLowerASCII(prev) || isUpperASCII(prev))
	})
	return strings.ToLower(name)
}

var pyKeywords = map[string]bool{
	"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true,
	"async": true, "await": true, "break": true, "class": true, "continue": true,
	"def": true, "del": true, "elif": true, "else": true, "except": true, "finally": true,
	"for": true, "from": true, "global": true, "if": true, "import": true, "in": true,
	"is": true, "lambda": true, "nonlocal": true, "not": true, "or": true, "pass": true,
	"raise": true, "return": true, "try": true, "while": true, "with": true, "yield": true,
}

// pySafeSnakeCase is casing.safe_snake_case: snake_case, then a
// trailing "_" for a keyword.
func pySafeSnakeCase(s string) string {
	s = pySnakeCase(s)
	if pyKeywords[s] {
		return s + "_"
	}
	if !isPyIdent(s) {
		return "_" + s
	}
	return s
}

func isPyIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || isUpperASCII(c) || isLowerASCII(c) || (i > 0 && isDigitASCII(c))) {
			return false
		}
	}
	return true
}
