package app

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 浏览器抓取后的模型校验 + 302 自愈。
//
// 用户要求（2026-09-16）：
//  1. 每次浏览器抓完 cookie，立刻用**设置里的默认模型**发一次真实请求校验；
//  2. 校验失败（502）→ 重新抓一次（说明抓到的 cookie 不可用）；
//  3. 校验返回 302（出口被 Google 反爬拦）→ 若有代理池，先把代理池失败计数
//     全部重置（让被熔断的出口立即回池），再重试一次；
//  4. 若仍 302 → 返回结构化错误，前端弹窗给出具体解决方法。
//
// 为什么必须做：抓取成功 ≠ cookie 可用。浏览器里 cookie 看着齐全、写进池子，
// 但出口 IP 正被 Google 拦（302 sorry 页）时，请求全挂 —— 面板上却显示「已入池」。
// 这层校验把「抓到了」和「真能用」之间的缝隙补上。

// BrowserVerifyResult 是抓取后校验的结构化结果，前端据此弹窗。
type BrowserVerifyResult struct {
	OK      bool   `json:"ok"`
	Status  string `json:"status"` // success / fetch_failed / proxy_blocked / model_failed / no_model
	Model   string `json:"model"`
	Detail  string `json:"detail"`
	Retried bool   `json:"retried"` // 是否经历过「重置代理池后重试」
	// Hint 是给用户看的解决方法（status=proxy_blocked 时必填）。
	Hint string `json:"hint,omitempty"`
	// ProxyReset 表示这次校验过程中重置过代理池失败计数。
	ProxyReset bool `json:"proxy_reset"`
}

// defaultModelForVerify 取设置里的默认模型；不可用时退回 gemini-3.6-flash。
func defaultModelForVerify() string {
	m := strings.TrimSpace(rtCfg().DefaultModel)
	if m == "" {
		return "gemini-3.6-flash"
	}
	if _, _, err := resolveModel(m); err != nil {
		// 默认模型不可用（例如需要 cookie 但池子空了）时退回一个基础模型，
		// 校验目的是「这条链路通不通」，不是「模型高级不高级」。
		return "gemini-3.6-flash"
	}
	return m
}

// verifyCookieByModel 用默认模型发一次真实请求，判断刚入库的 cookie 能不能用。
//
// 返回的 error 只在「网络层彻底失败」时非空；模型层面的失败（502/302）都体现在
// BrowserVerifyResult 里，方便前端统一处理。
func verifyCookieByModel() (BrowserVerifyResult, error) {
	model := defaultModelForVerify()
	mc, ok := Models[model]
	if !ok {
		return BrowserVerifyResult{Status: "no_model", Model: model,
			Detail: "默认模型不可用，跳过校验"}, nil
	}
	// 校验请求走完整链路（含 cookie 挑选、出口选择），带 3 次重试太多 ——
	// 校验只想知道「现在能不能用」，一次就够，失败由外层决定重抓。
	res, err := probeWithModel(model, mc)
	vr := BrowserVerifyResult{Model: model}
	if err != nil {
		vr.Status = "model_failed"
		vr.Detail = err.Error()
		return vr, nil
	}
	if res == nil {
		vr.Status = "model_failed"
		vr.Detail = "校验请求没有返回结果"
		return vr, nil
	}
	vr.OK = true
	vr.Status = "success"
	vr.Detail = fmt.Sprintf("默认模型 %s 校验通过（上游自报 %s）", model, res.UpstreamModel)
	return vr, nil
}

// probeWithModel 发一次单次尝试的模型请求（不重试），返回结果。
func probeWithModel(model string, mc ModelConfig) (*StreamResult, error) {
	// 复用主流程但把重试压到 1 次：校验失败要快速返回，让外层决定重抓。
	saved := rtCfg().RetryAttempts
	if saved > 1 {
		saveRuntimeConfigPatch(map[string]interface{}{"retry_attempts": 1})
		defer saveRuntimeConfigPatch(map[string]interface{}{"retry_attempts": saved})
	}
	_, _, res, err := callGemini("Say OK", "Say OK", mc, nil, nil, nil, nil)
	return res, err
}

// saveRuntimeConfigPatch 局部更新运行时配置（仅供内部临时调整用）。
func saveRuntimeConfigPatch(patch map[string]interface{}) {
	c := rtCfg()
	if v, ok := patch["retry_attempts"].(int); ok {
		c.RetryAttempts = v
	}
	_ = saveRuntimeConfig(c)
}

// errProxyBlocked 表示出口被上游反爬拦截（302 sorry 页）。
var errProxyBlocked = errors.New("出口被 Google 反爬拦截（302 sorry 页）")

// isProxyBlockedError 判断错误串是不是出口被拦。
func isProxyBlockedError(msg string) bool {
	return strings.Contains(msg, "sorry/index") || strings.Contains(msg, "/sorry/") ||
		strings.Contains(msg, "302")
}

// resetAllProxyFailures 把代理池里所有出口的失败计数清零。
//
// 为什么是整个池子：被 Google 拦的出口其实还能用（106-121 分钟自恢复），
// 熔断计数把它们挡在池外反而让流量全压到剩余出口上、加速全部被拦。
// 302 说明「现在这个时刻出口被拦」，重置后让调度重新试一遍所有出口，
// 往往能挑到一个没被拦的。失败计数本来也会在成功时清零，重置没有副作用。
func resetAllProxyFailures() int {
	_, err := getDB().Exec(`UPDATE proxies SET fail_count=0, last_error='' WHERE fail_count > 0`)
	if err != nil {
		logf("[verify] 重置代理池失败计数出错: %v", err)
		return 0
	}
	loadProxies()
	n := 0
	proxyMu.RLock()
	for _, p := range proxyCache {
		if p.FailCount == 0 {
			n++
		}
	}
	proxyMu.RUnlock()
	logf("[verify] 已重置代理池失败计数（%d 个出口可重新参与调度）", n)
	return n
}

// browserVerifyAfterFetch 是抓取后的完整校验流程，按用户要求的顺序执行：
//
//	抓取成功 → 模型校验
//	  ├─ 通过 → 返回 ok
//	  ├─ 502/模型失败 → 重抓一次 → 再校验（最多一次）
//	  └─ 302（出口被拦）→ 重置代理池 → 再校验 → 仍 302 则返回带 Hint 的失败
func browserVerifyAfterFetch(label, profile string, canRefetch bool) BrowserVerifyResult {
	vr, _ := verifyCookieByModel()
	if vr.OK {
		return vr
	}

	// 302：出口被拦，先重置代理池（若有），再试一次。
	if isProxyBlockedError(vr.Detail) {
		if _, total := proxyPoolSize(); total > 0 {
			resetAllProxyFailures()
			vr.ProxyReset = true
			vr.Retried = true
			if vr2, _ := verifyCookieByModel(); vr2.OK {
				vr2.ProxyReset = vr.ProxyReset
				vr2.Retried = true
				vr2.Detail += "（重置代理池后恢复）"
				return vr2
			} else {
				vr = vr2
				vr.ProxyReset = true
				vr.Retried = true
			}
		}
		// 仍失败：给出可操作的解决方法。
		vr.Status = "proxy_blocked"
		vr.Hint = proxyBlockedHint(vr.ProxyReset)
		return vr
	}

	// 非 302 的模型失败：抓到的 cookie 可能有问题，重抓一次再校验。
	if canRefetch {
		logf("[verify] 默认模型校验失败（%s），重新抓取 profile %q", vr.Detail, profile)
		if ok, _, err := browserRefreshOneForce(label, profile); ok && err == nil {
			vr.Retried = true
			if vr2, _ := verifyCookieByModel(); vr2.OK {
				vr2.Retried = true
				vr2.Detail += "（重抓后恢复）"
				return vr2
			} else {
				vr2.Retried = true
				vr2.Status = "model_failed"
				return vr2
			}
		} else if err != nil {
			vr.Detail += fmt.Sprintf("；重抓也失败：%v", err)
		}
	}
	vr.Status = "model_failed"
	if vr.Hint == "" {
		vr.Hint = "cookie 已入库但默认模型请求失败。请到「Cookie 池」点「检测」查看具体原因；" +
			"若提示会话票过期，稍候自动续票即可；若持续失败请到 VNC 桌面确认账号登录状态。"
	}
	return vr
}

// proxyPoolSize 返回 (enabled 数, 总数)。
func proxyPoolSize() (int, int) {
	proxyMu.RLock()
	defer proxyMu.RUnlock()
	n, en := 0, 0
	for _, p := range proxyCache {
		n++
		if p.Enabled {
			en++
		}
	}
	return en, n
}

// proxyBlockedHint 拼「302 被拦」的解决方法（弹窗正文）。
func proxyBlockedHint(reset bool) string {
	var b strings.Builder
	b.WriteString("出口 IP 被 Google 反爬拦截（302 跳转到 sorry 页），这不是 cookie 的问题 —— ")
	b.WriteString("同一条链路换一个出口 IP 通常立即恢复。按下面顺序处理：\n\n")
	if reset {
		b.WriteString("1. 【已自动执行】重置了代理池失败计数，让被熔断的出口重新参与调度，但没有可用的干净出口。\n")
	} else {
		b.WriteString("1. 当前没有配置代理池（或池为空），请求走的是本机直连 IP —— 直连 IP 被拦时只能等或换网络。\n")
	}
	b.WriteString("2. 到「代理池」页面：确认至少有一个出口的 URL 可用、状态是启用；")
	b.WriteString("点各出口的「重置」清掉失败计数，再点「检测」看哪个出口能通。\n")
	b.WriteString("3. 如果所有出口都被拦：这是 IP 信誉问题，通常 1~2 小时自动解除；")
	b.WriteString("急用的话换一个代理节点（住宅 IP 效果最好），或稍后再试。\n")
	b.WriteString("4. 验证是否恢复：到「Cookie 池」点对应账号的「检测」，看到「登录态有效」即已恢复。")
	return b.String()
}

// browserRefreshOneForce 是绕开冷却闸门的一次强制抓取，仅供校验失败后的自动重抓。
//
// 与 browserRefreshOne 的差别只有闸门：校验失败说明刚抓的那份确实不能用，
// 此时冷却闸门（防的是「多处并发刷新」）不该挡住这次有针对性的重抓。
// 但重抓后仍会把冷却时间戳刷新，避免紧接着又被其它路径触发。
func browserRefreshOneForce(label, profile string) (bool, string, error) {
	if !browserEnabled() {
		return false, "", fmt.Errorf("浏览器登录未启用")
	}
	// 记录一次尝试时间（保持冷却语义），但不拦截。
	browserRefreshMu.Lock()
	browserLastRefreshAt[profile] = time.Now().Unix()
	browserRefreshMu.Unlock()
	return browserRefreshCore(label, profile)
}
