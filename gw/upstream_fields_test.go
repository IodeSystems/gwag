package gateway

import "testing"

func TestUpstreamArgPath(t *testing.T) {
	for in, want := range map[string]string{
		"body.depends[0].lines": "body.depends.0.lines",
		"body.items[1].tags[2]": "body.items.1.tags.2",
		"path.id":               "id",
		"query.limit":           "limit",
		"body":                  "body",
	} {
		if got := upstreamArgPath(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

func TestUpstreamFieldErrors(t *testing.T) {
	got := upstreamFieldErrors([]byte(`{"title":"Unprocessable Entity","status":422,"errors":[{"location":"body.items[1].name","message":"too short"},{"message":"no location"}]}`))
	if len(got) != 1 || got[0].Path != "body.items.1.name" || got[0].Message != "too short" {
		t.Fatalf("got %+v", got)
	}
	if got := upstreamFieldErrors([]byte(`not json`)); got != nil {
		t.Fatalf("non-JSON body: %+v", got)
	}
}
