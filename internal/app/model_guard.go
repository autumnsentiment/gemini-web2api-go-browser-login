package app

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 模型一致性检测（model guard）。
//
// 上游有个静默行为：cookie 的会话票旧了/会话被当匿名后，Gemini 不报错、只是把
// 请求当匿名处理 —— 3.1 Pro 静默降级成 3.5 Flash-Lite、思考链消失，HTTP 照样
// 200，客户端完全看不出来。这是「票过期」最早、最可靠的信号：请求记录里已经存
// 了「请求的模型」和「上游自报的实际模型」（响应帧 [42] 自报，extractUpstreamModel），
// 两者一对比就能发现。
//
// 判据故意保守，两个条件必须同时成立：
//   1. 请求的是登录态专属能力模型（3.1 Pro / 3.7 Flash / thinking / 媒体工具）；
//   2. 上游自报的却是匿名档（3.5 Flash-Lite，实测的静默降级落点）。
//
// 满足后触发 browserRefreshOne 重抓该账号的 cookie：它自带 120s 冷却 + kv 排程
// + 并发单飞，连续触发也只有第一个真正去抓，不会打断反风控节奏。触发本身另加
// 一层外层防抖，避免每条降级请求都白拿锁。

var (
	modelGuardMu        sync.Mutex
	modelGuardLastFired = map[int64]int64{} // 账号ID -> 上次触发的时刻
)

// modelGuardCooldownSec 同一账号两次「模型不一致 → 重抓」的最小间隔（外层防抖）。
//
// ★ 5 小时（2026-09-13 调整）★
//
// Gemini 对每个账号有 **5 小时滚动用量窗口 + 周限量**。guard 一旦触发，说明这个
// 账号刚才已经消耗过配额了（会话被当匿名前那些请求都算在窗口里）——窗口内反复
// 检出降级、反复续票/重抓没有任何意义：票是好的，配额就是耗尽状态，等 5 小时
// 窗口滚动过去自然恢复。把冷却设成 5 小时正好对齐用量窗口：一次触发只救一次，
// 之后整个窗口期安静，既不再打扰上游，也不给浏览器增加风控暴露。
const modelGuardCooldownSec = 5 * 60 * 60

// modelGuardFallbackRe 匹配「上游自报模型」里的匿名档名字。
// 3.5 Flash-Lite 是已知的静默降级落点（3.1 Pro / thinking / 媒体工具匿名时全部
// 落到它，见 resolveModel / Models 的注释）。
var modelGuardFallbackRe = regexp.MustCompile(`(?i)flash[- ]?lite`)

// modelGuardPremiumHints 是登录态专属模型的请求名（不含 @think= 后缀）。
// 判据用请求名而不是 ModelConfig 字段：guard 挂在 recordRequest 后面，只有名字。
var modelGuardPremiumHints = map[string]bool{
	"gemini-3.1-pro":                 true,
	"gemini-3.7-flash":               true,
	"gemini-3.1-pro-thinking":        true,
	"gemini-3.5-flash-lite-thinking": true,
	"gemini-3.6-flash-thinking":      true,
	"gemini-3.7-flash-thinking":      true,
	"gemini-image":                   true,
	"gemini-music":                   true,
	"gemini-video":                   true,
	"gemini-canvas":                  true,
}

// modelGuardCheck 对一条已完成的请求做模型一致性检测。
//
// model:    客户端请求的模型名（已剥 @think= 后缀）
// upstream: 上游自报的显示名（可为空——取不到就不判，避免误报）
// accountID: 本次用的 cookie 账号，0 = 匿名请求（匿名降级是预期行为，跳过）
// status:   HTTP 结果，只看 200
func modelGuardCheck(model, upstream string, accountID int64, status int) {
	if status != 200 || accountID <= 0 || upstream == "" {
		return
	}
	// 请求名也剥一次后缀，客户端可能带 @think=2
	if idx := strings.Index(model, "@"); idx >= 0 {
		model = model[:idx]
	}
	if !modelGuardPremiumHints[model] {
		return
	}
	if !modelGuardFallbackRe.MatchString(upstream) {
		return
	}

	modelGuardMu.Lock()
	now := time.Now().Unix()
	if last, ok := modelGuardLastFired[accountID]; ok {
		if remain := modelGuardCooldownSec - (now - last); remain > 0 {
			modelGuardMu.Unlock()
			logf("[model-guard] 账号 #%d 检测冷却中（剩约 %d 小时 %d 分钟），跳过：5 小时用量窗口内重复触发无意义", accountID, remain/3600, remain%3600/60)
			return
		}
	}
	modelGuardLastFired[accountID] = now
	modelGuardMu.Unlock()

	logf("[model-guard] 账号 #%d 模型不一致：请求 %s 但上游实际用了 %s（静默降级），触发重新抓取 cookie", accountID, model, upstream)
	go modelGuardRefetch(accountID, model, upstream)
}

// modelGuardRefetch 后台重抓触发账号的 cookie。
func modelGuardRefetch(accountID int64, model, upstream string) {
	a := accountByID(accountID)
	if a == nil {
		return
	}
	// 票据续新优先（最便宜，不打扰浏览器）：上游 #25 的哨兵流程换发
	// __Secure-1PSIDTS（约 30 分钟过期的那张短命票），很多时候票只是旧了没死，
	// 续完就恢复。rotateAccount 失败时 refreshes 为空，fallback 走浏览器重抓。
	proxyURL := ""
	if a.ProxyID > 0 {
		proxyURL = proxyURLByID(a.ProxyID)
	}
	_, refreshed, rerr := tryRotate1PSIDTS(a.ID, a.Cookie, proxyURL)
	if rerr == nil && len(refreshed) > 0 {
		if fresh := accountByID(a.ID); fresh != nil && fresh.Cookie != a.Cookie {
			invalidateXSRF(fresh.Cookie)
			logf("[model-guard] 账号 #%d 已续票（%s），待后续请求验证恢复", accountID, strings.Join(refreshed, ", "))
			return
		}
	}
	if rerr != nil {
		logf("[model-guard] 账号 #%d 续票失败（%v），转浏览器重抓", accountID, rerr)
	}
	// 续不了（401/400）或没换出新值：会话可能真死了，走浏览器重抓。
	// browserRefreshOne 自带冷却闸门；拿不到锁/冷却中会直接返回错误，不强抢。
	if a.Source == "browser" && a.Profile != "" {
		ok, detail, err := browserRefreshOne(a.Label, a.Profile)
		if err != nil {
			logf("[model-guard] 账号 #%d 重抓未完成（%s），稍后由定时刷新接手: %v", accountID, detail, err)
			return
		}
		if ok {
			logf("[model-guard] 账号 #%d cookie 已重抓入库（触发：%s → %s）", accountID, model, upstream)
		}
		return
	}
	// 非 browser 来源（手工导入）：没有浏览器可抓，把嫌疑写到面板上让人处理。
	_, _ = getDB().Exec(`UPDATE accounts SET last_error=? WHERE id=?`,
		fmt.Sprintf("模型不一致（请求 %s 上游用 %s）：会话疑似已匿名，请更新该 cookie",
			model, upstream), accountID)
}
