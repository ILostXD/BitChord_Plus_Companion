package main

import "testing"

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"1.0.1", "1.0.0", true}, {"1.0.0", "1.0.0", false}, {"1.0.0", "1.0.0-alpha.9", true},
		{"1.0.0-alpha.9", "1.0.0", false}, {"1.10.0", "1.9.3", true}, {"v2.0", "1.99.0", true}, {"1.0.0", "1.1.0", false},
	} {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
