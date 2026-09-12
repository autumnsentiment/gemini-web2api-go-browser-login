package app

import (
	"errors"
	"strings"
	"testing"
)

// 真根因回归：__Secure-1PSIDTS 过期会让 /app 变成「匿名单页」，表现为
// "no SNlM0e in page"。这种错误必须被认成「票旧了」（可以先续票复检），
// 而不是「号没了」。
func TestStaleSessionDetection(t *testing.T) {
	if !looksLikeStaleSession(errors.New("no SNlM0e in page (cookie expired or not signed in)")) {
		t.Error("no SNlM0e 应被判为票旧（可续票复检）")
	}
	for _, e := range []string{
		"fetch /app: HTTP 302 -> https://www.google.com/sorry/index",
		"fetch /app: HTTP 401",
		`Get "https://gemini.google.com/app": EOF`,
	} {
		if looksLikeStaleSession(errors.New(e)) {
			t.Errorf("%q 不该被判为票旧", e)
		}
	}
	if looksLikeStaleSession(nil) {
		t.Error("nil 不该被判为票旧")
	}
}

// 结论文案要指向「续票可恢复」，别再把用户往「重新登录」上引（那是过度反应）。
func TestExplainStalePointsToRenew(t *testing.T) {
	got := explainCookieFailure(errors.New("no SNlM0e in page (cookie expired or not signed in)"))
	if !strings.Contains(got, "票") {
		t.Errorf("解释文案没提到会话票：%q", got)
	}
	if !strings.Contains(got, "续票") {
		t.Errorf("解释文案没给「续票」这个可操作结论：%q", got)
	}
}
