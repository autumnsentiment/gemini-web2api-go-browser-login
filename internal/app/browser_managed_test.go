package app

import "testing"

func TestBrowserManagedCookie(t *testing.T) {
	cases := []struct {
		source string
		want   bool
	}{
		{"browser", true},
		{"remote", true},
		{"manual", false},
		{"", false},
	}
	for _, c := range cases {
		if got := browserManagedCookie(CookieAccount{Source: c.source}); got != c.want {
			t.Errorf("browserManagedCookie(%q) = %v, want %v", c.source, got, c.want)
		}
	}
}
