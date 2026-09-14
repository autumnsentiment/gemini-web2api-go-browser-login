package app

import (
	"strings"
	"testing"
)

// 判据矩阵：只有「登录态专属模型 + 上游自报匿名档 + 带 cookie 账号 + 200」才触发。
func TestModelGuardJudgement(t *testing.T) {
	cases := []struct {
		name     string
		model    string
		upstream string
		acctID   int64
		status   int
		want     bool
	}{
		{"降级命中：3.1pro→flash-lite", "gemini-3.1-pro", "3.5 Flash-Lite", 7, 200, true},
		{"降级命中：thinking→flash-lite", "gemini-3.6-flash-thinking", "3.5 Flash-Lite", 7, 200, true},
		{"降级命中：媒体工具→flash-lite", "gemini-image", "3.5 Flash-Lite", 7, 200, true},
		{"降级命中：带@think后缀", "gemini-3.1-pro@think=2", "3.5 Flash-Lite", 7, 200, true},
		{"降级命中：媒体→Flash Lite无连字符", "gemini-music", "3.5 Flash Lite", 7, 200, true},
		{"正常：上游自报正确模型", "gemini-3.1-pro", "3.1 Pro", 7, 200, false},
		{"上游自报为空不判", "gemini-3.1-pro", "", 7, 200, false},
		{"非登录态模型不判", "gemini-3.6-flash", "3.5 Flash-Lite", 7, 200, false},
		{"匿名请求(账号0)不判", "gemini-3.1-pro", "3.5 Flash-Lite", 0, 200, false},
		{"非200不判", "gemini-3.1-pro", "3.5 Flash-Lite", 7, 502, false},
		{"3.7flash降级命中", "gemini-3.7-flash", "3.5 Flash-Lite", 7, 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelGuardShouldFire(tc.model, tc.upstream, tc.acctID, tc.status); got != tc.want {
				t.Errorf("modelGuardShouldFire(%q, %q, %d, %d) = %v, want %v",
					tc.model, tc.upstream, tc.acctID, tc.status, got, tc.want)
			}
		})
	}
}

// modelGuardShouldFire 把 modelGuardCheck 的判定部分拆出来供测试；
// modelGuardCheck 本体只多做了防抖和触发动作。
func modelGuardShouldFire(model, upstream string, accountID int64, status int) bool {
	if status != 200 || accountID <= 0 || upstream == "" {
		return false
	}
	if idx := strings.Index(model, "@"); idx >= 0 {
		model = model[:idx]
	}
	if !modelGuardPremiumHints[model] {
		return false
	}
	return modelGuardFallbackRe.MatchString(upstream)
}
