package ir

import (
	"reflect"
	"testing"
)

func TestGraphQLEnumValueNames(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"A", "b_c", "Ok1"}, []string{"A", "b_c", "Ok1"}},
		{[]string{"true", "false", "null"}, []string{"TRUE", "FALSE", "NULL"}},
		{[]string{"a-b", "a b", "a_b"}, []string{"a_b_3", "a_b_2", "a_b"}},
		{[]string{"1st", "", "__x", "___", "é"}, []string{"_1st", "_", "_x", "_2", "_3"}},
		{[]string{"TRUE", "true"}, []string{"TRUE", "TRUE_2"}},
		{[]string{"x", "x"}, []string{"x", "x"}},
		{[]string{"a-b", "a-b"}, []string{"a_b", "a_b"}},
	}
	for _, c := range cases {
		if got := GraphQLEnumValueNames(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("GraphQLEnumValueNames(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
