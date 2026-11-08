package events

import (
	"reflect"
	"testing"
)

func TestParsePattern(t *testing.T) {
	p, err := ParsePattern("events.orders.{orderId}.*.>")
	if err != nil {
		t.Fatal(err)
	}
	want := []Segment{{"orderId", SegmentNamed}, {"P1", SegmentPositional}, {"Rest", SegmentRest}}
	if got := p.Segments(); !reflect.DeepEqual(got, want) {
		t.Fatalf("segments = %v, want %v", got, want)
	}
	if got := p.Wildcard(); got != "events.orders.*.*.>" {
		t.Fatalf("wildcard = %q", got)
	}
	s, err := p.Subject([]string{"o1", "eu", "a.b"})
	if err != nil {
		t.Fatal(err)
	}
	if s != "events.orders.o1.eu.a.b" {
		t.Fatalf("subject = %q", s)
	}
	vals, ok := p.Match(s)
	if !ok || !reflect.DeepEqual(vals, []string{"o1", "eu", "a.b"}) {
		t.Fatalf("match = %v %v", vals, ok)
	}
}

func TestParsePatternErrors(t *testing.T) {
	for _, p := range []string{"", "a..b", "a.>.b", "a.{1x}", "a.{x}.{x}", "a.b*", "a.{x"} {
		if _, err := ParsePattern(p); err == nil {
			t.Errorf("ParsePattern(%q): no error", p)
		}
	}
}

func TestPatternSubjectRejectsBadValues(t *testing.T) {
	p, _ := ParsePattern("a.{x}")
	for _, v := range []string{"", "b.c", "*", ">", "a b"} {
		if _, err := p.Subject([]string{v}); err == nil {
			t.Errorf("Subject(%q): no error", v)
		}
	}
	if _, err := p.Subject(nil); err == nil {
		t.Error("Subject(nil): no error")
	}
}

func TestPatternMatch(t *testing.T) {
	p, _ := ParsePattern("a.*.c")
	for subj, want := range map[string]bool{"a.b.c": true, "a.b": false, "a.b.c.d": false, "x.b.c": false} {
		if _, ok := p.Match(subj); ok != want {
			t.Errorf("Match(%q) = %v, want %v", subj, ok, want)
		}
	}
	q, _ := ParsePattern("a.>")
	if _, ok := q.Match("a"); ok {
		t.Error(`"a.>" matched "a"`)
	}
}

func TestStreamName(t *testing.T) {
	if got := StreamName("orders.v1.OrderEvents"); got != "EV_orders_v1_OrderEvents" {
		t.Fatal(got)
	}
}
