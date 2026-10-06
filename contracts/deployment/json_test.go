package deployment

import (
	"errors"
	"testing"
)

func TestCanonicalBytes(t *testing.T) {
	input := []byte(" { \"z\" : [2,1], \"a\":\"<>&\\u2028\\u2029\\n😀\", \"é\":true }")
	want := "{\"a\":\"\\u003c\\u003e\\u0026\\u2028\\u2029\\n😀\",\"z\":[2,1],\"é\":true}"
	got, err := CanonicalJSON(input)
	if err != nil || string(got) != want {
		t.Fatalf("got %s %v, want %s", got, err, want)
	}
	again, err := CanonicalJSON(got)
	if err != nil || string(again) != want {
		t.Fatal("canonicalization is not idempotent", err)
	}
	if Digest([]byte("abc")) != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("SHA-256 vector")
	}
}
func TestStrictJSON(t *testing.T) {
	cases := []struct{ input, rule string }{
		{`{"a":1,"\u0061":2}`, "JSON_DUPLICATE"}, {`{"x":{"a":1,"a":2}}`, "JSON_DUPLICATE"},
		{`"\ud800"`, "JSON_UNICODE"}, {`"\udc00"`, "JSON_UNICODE"}, {string([]byte{34, 255, 34}), "JSON_UNICODE"},
		{`1e1`, "JSON_NUMBER"}, {`1.0`, "JSON_NUMBER"}, {`-0`, "JSON_NUMBER"}, {`9007199254740992`, "JSON_NUMBER"},
		{`{} {}`, "JSON_SYNTAX"}, {`[1,]`, "JSON_SYNTAX"},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			_, err := CanonicalJSON([]byte(c.input))
			var v *Violation
			if !errors.As(err, &v) || v.Rule != c.rule {
				t.Fatalf("got %v, want %s", err, c.rule)
			}
		})
	}
}
func TestSelectorSetRule(t *testing.T) {
	for _, c := range []struct {
		a, b map[string]string
		want bool
	}{
		{map[string]string{"app": "api"}, map[string]string{"role": "backend"}, true},
		{map[string]string{"app": "api"}, map[string]string{"app": "api", "role": "backend"}, true},
		{map[string]string{"app": "api"}, map[string]string{"app": "worker"}, false},
	} {
		if overlap(c.a, c.b) != c.want {
			t.Fatal("incorrect equality selector intersection")
		}
	}
}
