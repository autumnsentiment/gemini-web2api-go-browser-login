package app

import (
	"errors"
	"strings"
	"testing"
)

func TestExplain302IsNotCookieFailure(t *testing.T) {
	cases := []struct {
		err  string
		want string
		not  string
	}{
		{"fetch /app: HTTP 302 -> https://www.google.com/sorry/index?continue=https://gemini.google.com/app",
			"反爬", "登录页"},
		{"fetch /app: HTTP 302 -> https://accounts.google.com/ServiceLogin?continue=...",
			"登录页", ""},
		{"fetch /app: HTTP 302 -> https://consent.google.com/m?continue=...",
			"同意页", ""},
	}
	for _, c := range cases {
		got := explainCookieFailure(errors.New(c.err))
		if !strings.Contains(got, c.want) {
			t.Errorf("explainCookieFailure(%q) = %q, 期望含 %q", c.err, got, c.want)
		}
		if c.not != "" && strings.Contains(got, c.not) {
			t.Errorf("explainCookieFailure(%q) = %q, 不该含 %q", c.err, got, c.not)
		}
	}

	// sorry 302 / 普通 302 一律不能计入 cookie 健康度；只有真登录跳转另说（也不计入）
	for _, e := range []string{
		"fetch /app: HTTP 302 -> https://www.google.com/sorry/index",
		"fetch /app: HTTP 302 -> https://accounts.google.com/ServiceLogin",
		`Get "https://gemini.google.com/app": EOF`,
	} {
		if st := xsrfAuthStatus(errors.New(e)); st != 0 {
			t.Errorf("xsrfAuthStatus(%q) = %d, 期望 0（不累加 fail_count）", e, st)
		}
	}
	// 401/403 仍要认
	for _, e := range []string{"fetch /app: HTTP 401", "fetch /app: HTTP 403"} {
		if st := xsrfAuthStatus(errors.New(e)); st != 401 {
			t.Errorf("xsrfAuthStatus(%q) = %d, 期望 401", e, st)
		}
	}
}
