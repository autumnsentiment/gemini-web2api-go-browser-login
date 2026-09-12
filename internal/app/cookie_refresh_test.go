package app

import (
	"strings"
	"testing"
)

// 服务端几乎每个响应都在刷新 SIDCC / __Secure-1PSIDCC / __Secure-3PSIDCC，
// 客户端要收下再带回去。一直发旧值会被判定为过期会话——实测号活一两小时就失效。
func TestMergeSetCookie(t *testing.T) {
	base := "SID=a; SAPISID=b; SIDCC=old1; __Secure-1PSIDCC=old2; __Secure-1PSID=c"

	got := mergeSetCookie(base, []string{
		"SIDCC=new1; expires=Sat, 07 Aug 2027 00:00:00 GMT; path=/; secure",
		"__Secure-1PSIDCC=new2; path=/; secure; httponly",
	})
	for _, want := range []string{"SIDCC=new1", "__Secure-1PSIDCC=new2"} {
		if !strings.Contains(got, want) {
			t.Errorf("没刷新 %q: %s", want, got)
		}
	}
	// 没被下发的项必须原样保留 —— 丢一个就等于把登录态削掉一块
	for _, want := range []string{"SID=a", "SAPISID=b", "__Secure-1PSID=c"} {
		if !strings.Contains(got, want) {
			t.Errorf("丢了 %q: %s", want, got)
		}
	}
	if strings.Contains(got, "old1") || strings.Contains(got, "old2") {
		t.Errorf("旧值没被替换掉: %s", got)
	}
}

// 响应里的新键要追加进来，不能丢。
func TestMergeSetCookieAddsNew(t *testing.T) {
	got := mergeSetCookie("SID=a", []string{"__Secure-3PSIDCC=fresh; path=/"})
	if !strings.Contains(got, "SID=a") || !strings.Contains(got, "__Secure-3PSIDCC=fresh") {
		t.Errorf("新键没追加: %s", got)
	}
}

// 删除指令（空值 + 1970 过期）不能当成新值写进去，否则会把好端端的 cookie 清空。
func TestMergeSetCookieIgnoresDeletion(t *testing.T) {
	got := mergeSetCookie("SID=a; SIDCC=keepme",
		[]string{"SIDCC=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/"})
	if !strings.Contains(got, "SIDCC=keepme") {
		t.Errorf("删除指令把值清掉了: %s", got)
	}
}

// 没有 Set-Cookie 时原样返回 —— 否则每次请求都会重排一遍 cookie 串，
// 让按内容比对的地方（poolHasCookie / --cookie-file 去重）误判成变了。
func TestMergeSetCookieNoop(t *testing.T) {
	base := "SID=a; SAPISID=b"
	if got := mergeSetCookie(base, nil); got != base {
		t.Errorf("空输入却改了串: %q", got)
	}
	if got := mergeSetCookie(base, []string{"garbage-without-equals"}); got != base {
		t.Errorf("无法解析的 Set-Cookie 却改了串: %q", got)
	}
}

// 值里带 '='（base64 补位）不能被切坏。
func TestMergeSetCookieKeepsEquals(t *testing.T) {
	got := mergeSetCookie("SID=a", []string{"SIDCC=AB==; path=/"})
	if !strings.Contains(got, "SIDCC=AB==") {
		t.Errorf("值里的等号被切坏: %s", got)
	}
}
