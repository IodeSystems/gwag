package main

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

type ctlCall struct {
	noun string
	args []string
}

// stubNouns replaces every ctl handler with a recorder that writes
// "out:<noun>" to stdout and returns 7, then restores them.
func stubNouns(t *testing.T) *[]ctlCall {
	t.Helper()
	var calls []ctlCall
	orig := ctlNouns
	stubbed := map[string]cmdFunc{}
	for n := range orig {
		n := n
		stubbed[n] = func(a []string, o, _ io.Writer) int {
			calls = append(calls, ctlCall{n, a})
			fmt.Fprint(o, "out:"+n)
			return 7
		}
	}
	ctlNouns = stubbed
	t.Cleanup(func() { ctlNouns = orig })
	return &calls
}

func TestLegacyAliasesMatchCtlNouns(t *testing.T) {
	calls := stubNouns(t)
	for old, noun := range legacyAliases {
		*calls = nil
		var out, errb bytes.Buffer
		code, ok := legacyMain(old, []string{"x", "--flag"}, &out, &errb)
		if !ok || code != 7 {
			t.Fatalf("%s: ok=%v code=%d", old, ok, code)
		}
		want := fmt.Sprintf("gwag: %q moved to \"gwag ctl %s\"; the old name goes away in the next release\n", old, noun)
		if errb.String() != want {
			t.Errorf("%s: stderr %q, want %q", old, errb.String(), want)
		}
		if out.String() != "out:"+noun {
			t.Errorf("%s: stdout %q", old, out.String())
		}
		if len(*calls) != 1 || (*calls)[0].noun != noun || !reflect.DeepEqual((*calls)[0].args, []string{"x", "--flag"}) {
			t.Errorf("%s: calls %v", old, *calls)
		}

		// The new name reaches the same handler, with no warning.
		*calls = nil
		out.Reset()
		errb.Reset()
		code = ctlMain([]string{noun, "x", "--flag"}, &out, &errb)
		if code != 7 || errb.Len() != 0 || out.String() != "out:"+noun {
			t.Errorf("ctl %s: code=%d out=%q err=%q", noun, code, out.String(), errb.String())
		}
		if len(*calls) != 1 || (*calls)[0].noun != noun {
			t.Errorf("ctl %s: calls %v", noun, *calls)
		}
	}
}

func TestLegacyServiceVerbs(t *testing.T) {
	calls := stubNouns(t)
	for _, v := range legacyServiceVerbs {
		*calls = nil
		var out, errb bytes.Buffer
		code, ok := legacyMain("service", []string{v, "--a"}, &out, &errb)
		if !ok || code != 7 {
			t.Fatalf("service %s: ok=%v code=%d", v, ok, code)
		}
		want := fmt.Sprintf("gwag: %q moved to \"gwag ctl services %s\"; the old name goes away in the next release\n", "service "+v, v)
		if errb.String() != want {
			t.Errorf("service %s: stderr %q", v, errb.String())
		}
		if len(*calls) != 1 || (*calls)[0].noun != "services" || !reflect.DeepEqual((*calls)[0].args, []string{v, "--a"}) {
			t.Errorf("service %s: calls %v", v, *calls)
		}
	}
}

func TestLegacyServiceUnknownVerb(t *testing.T) {
	calls := stubNouns(t)
	for _, args := range [][]string{nil, {"bogus"}} {
		var out, errb bytes.Buffer
		code, ok := legacyMain("service", args, &out, &errb)
		if !ok || code != 2 || out.Len() != 0 || len(*calls) != 0 {
			t.Errorf("%v: ok=%v code=%d out=%q calls=%v", args, ok, code, out.String(), *calls)
		}
		if !strings.Contains(errb.String(), "Usage:") || strings.Contains(errb.String(), "goes away") {
			t.Errorf("%v: stderr %q", args, errb.String())
		}
	}
}

func TestLegacyMainIgnoresOtherNames(t *testing.T) {
	stubNouns(t)
	for _, n := range []string{"up", "ctl", "bogus", ""} {
		if _, ok := legacyMain(n, nil, io.Discard, io.Discard); ok {
			t.Errorf("%q treated as alias", n)
		}
	}
}

func TestEveryAliasTargetsARealNoun(t *testing.T) {
	for old, noun := range legacyAliases {
		if _, ok := ctlNouns[noun]; !ok {
			t.Errorf("alias %s -> missing noun %s", old, noun)
		}
	}
	for noun := range ctlNouns {
		if _, ok := legacyAliases[noun]; !ok {
			t.Errorf("noun %s has no legacy alias", noun)
		}
	}
}

func TestHelpHidesAliasesAndListsNouns(t *testing.T) {
	var out, errb bytes.Buffer
	if code := ctlMain([]string{"help"}, &out, &errb); code != 0 || errb.Len() != 0 {
		t.Fatalf("ctl help: code=%d err=%q", code, errb.String())
	}
	for noun := range ctlNouns {
		if !strings.Contains(out.String(), "  "+noun+" ") {
			t.Errorf("ctl help does not list %s", noun)
		}
	}
	for _, old := range legacyNames() {
		if old == "service" || old == "services" {
			continue
		}
		if strings.Contains(topUsage, "gwag "+old+" ") {
			t.Errorf("top usage lists old name %q", old)
		}
	}
	for _, v := range legacyServiceVerbs {
		if strings.Contains(topUsage, "gwag service "+v) {
			t.Errorf("top usage lists legacy gwag service %s", v)
		}
	}
	if strings.Contains(topUsage, "gwag peer") {
		t.Error("top usage lists a moved command")
	}
	if !strings.Contains(topUsage, "gwag ctl") {
		t.Error("top usage does not mention gwag ctl")
	}
}

func TestCtlUnknownNounAndNoArgs(t *testing.T) {
	stubNouns(t)
	var out, errb bytes.Buffer
	if code := ctlMain(nil, &out, &errb); code != 2 || out.Len() != 0 || !strings.Contains(errb.String(), "Usage: gwag ctl") {
		t.Errorf("no args: code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	errb.Reset()
	if code := ctlMain([]string{"bogus"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), `unknown noun "bogus"`) {
		t.Errorf("unknown: code=%d err=%q", code, errb.String())
	}
}

// The real services handler: profile verbs and the old alias give the
// same stdout and exit code; only the alias warns.
func TestServicesSchemaViaCtlAndAlias(t *testing.T) {
	var newOut, newErr, oldOut, oldErr bytes.Buffer
	c1 := ctlMain([]string{"services", "schema"}, &newOut, &newErr)
	c2, ok := legacyMain("service", []string{"schema"}, &oldOut, &oldErr)
	if !ok || c1 != 0 || c2 != 0 {
		t.Fatalf("codes %d %d ok=%v", c1, c2, ok)
	}
	if newOut.Len() == 0 || newOut.String() != oldOut.String() {
		t.Error("stdout differs between ctl services schema and gwag service schema")
	}
	if newErr.Len() != 0 || strings.Count(oldErr.String(), "\n") != 1 {
		t.Errorf("stderr new=%q old=%q", newErr.String(), oldErr.String())
	}
}
