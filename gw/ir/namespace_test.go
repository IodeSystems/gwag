package ir

import "testing"

func TestSanitizeNamespace(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"Pets", "Pets"},                  // case preserved
		{"pets", "pets"},                  // already lower
		{"Pet Store", "Pet_Store"},        // space → underscore
		{"hello-world!", "hello_world"},   // dash → underscore, ! dropped
		{"My API", "My_API"},              // case preserved across words
		{"123abc", "_123abc"},             // leading digit guarded
		{"3D API", "_3D_API"},             // digit guard + case + space
		{"v1_api", "v1_api"},              // underscores kept
		{"!!!", ""},                       // nothing valid → empty
	}
	for _, tc := range cases {
		if got := SanitizeNamespace(tc.in); got != tc.want {
			t.Errorf("SanitizeNamespace(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
