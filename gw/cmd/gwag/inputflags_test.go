package main

import (
	"flag"
	"testing"
)

func TestInputFlags(t *testing.T) {
	for _, c := range []struct {
		args []string
		want int
	}{
		{nil, 0},
		{[]string{"--allow-lossy-coercion-of-inputs"}, 1},
		{[]string{"--legacy-null-args"}, 1},
		{[]string{"--allow-lossy-coercion-of-inputs", "--legacy-null-args"}, 2},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		f := addInputFlags(fs)
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		if got := len(f.options()); got != c.want {
			t.Errorf("%v: %d options, want %d", c.args, got, c.want)
		}
	}
}
