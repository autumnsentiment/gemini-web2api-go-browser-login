package app

import (
	"os"
	"testing"
)

// 用一份真实抓下来的联网搜索响应，验证 grounding 来源能被解析出来。
func TestExtractGrounding(t *testing.T) {
	raw, err := os.ReadFile("testdata/search_raw.txt")
	if err != nil {
		t.Skipf("没有 testdata/search_raw.txt，跳过: %v", err)
	}
	sources := extractGrounding(string(raw))
	if len(sources) == 0 {
		t.Fatal("这份响应应该有联网搜索来源，却一个都没解析出来")
	}
	for i, s := range sources {
		if s.URL == "" {
			t.Errorf("来源 %d 的 URL 为空", i)
		}
		if idx := len(s.URL); idx > 0 && contains(s.URL, "#:~:text=") {
			t.Errorf("来源 %d 的 URL 还带着 #:~:text= 片段: %s", i, s.URL)
		}
		t.Logf("来源 %d: %s | %s", i, s.Title, s.URL)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
