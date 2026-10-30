package ir

import (
	"encoding/json"
	"testing"
)

func TestValidScalarName(t *testing.T) {
	for name, want := range map[string]bool{
		"Cents": true, "_Money2": true,
		"": false, "Int": false, "Long": false, "__Cents": false, "2Cents": false, "Cents-USD": false,
	} {
		if got := validScalarName(name); got != want {
			t.Errorf("validScalarName(%q) = %v", name, got)
		}
	}
}

func TestSafeInt(t *testing.T) {
	for _, ok := range []any{int64(maxSafeInteger), -maxSafeInteger, float64(2500000000), "12", json.Number("7"), uint64(3)} {
		if _, got := safeInt(ok); !got {
			t.Errorf("safeInt(%v) refused", ok)
		}
	}
	for _, bad := range []any{maxSafeInteger + 1, 1.5, "1.5", "9007199254740992", uint64(1) << 60, true, nil} {
		if n, got := safeInt(bad); got {
			t.Errorf("safeInt(%v) = %d", bad, n)
		}
	}
}
