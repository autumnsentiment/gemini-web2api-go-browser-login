package app

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// 会话保活 / 续票。
//
// ★ 2026-09-13 换端点（线上实测）★
//
// 旧实现走「GET /RotateCookiesPage?og_pid=658 → 解析 init(...) → POST
// /RotateCookies with [658,"<init-id>"]」。实测 GET /RotateCookiesPage 现在直接回
//
//	HTTP 401 (Bad Request) "Error 401 (Bad Request)!!1"
//
// 对任何 cookie 都是 401 —— 这个页面入口已经被 Google 废掉，于是保活每 5 分钟
// 失败一次（[rotate] 保活失败: 取轮转页返回 HTTP 401），__Secure-1PSIDTS 永远
// 续不了新，10~20 分钟后票过期、/app 变匿名单页、整个池子被判死。
//
// 新流程（对齐 tools/cookie-sync 扩展里实测可用的那条）：在 gemini.google.com
// 的页面上下文里 POST https://accounts.google.com/RotateCookies，body 为
//
//	[{"cookieName":"__Secure-1PSIDTS","refreshStrategy":"ROTATE"}]
//
// 响应 Set-Cookie 下发新的 __Secure-1PSIDTS / __Secure-3PSIDTS（连带 SIDCC 族）。
// 注意 Origin/Referer 必须是 gemini.google.com：旧的 accounts.google.com Origin
// 是从废弃 iframe 抓包里抄的，跟着旧流程一起换掉。
//
// 对死会话这个端点回 400（[["er",…400…],["di",…]]）而不是 401，跟「真的不能续」
// 的语义一致；429 = 换得太勤，不是失败，照服务端建议等下一轮。

const (
	rotatePostURL = "https://accounts.google.com/RotateCookies"
	// og_pid 是产品标识，Gemini 固定 658。只用于日志辨识，新端点已不需要它。
	rotateProductID = 658
	// 服务端没给出间隔时的兜底值。
	defaultRotateInterval = 10 * time.Minute
)

// rotateTicketBody 让服务端轮换 __Secure-1PSIDTS（顺带 __Secure-3PSIDTS）。
// body 形状逐字取自扩展 forceRotateCookies()（tools/cookie-sync/ext-src/background.js）。
const rotateTicketBody = `[{"cookieName":"__Secure-1PSIDTS","refreshStrategy":"ROTATE"}]`

// rotateAccount 给一个账号做一次保活+续票，返回建议的下次间隔。
//
// 失败不抛给健康度（调用方 rotateAllAccounts 已经不记），这里只打日志。
func rotateAccount(a CookieAccount) (time.Duration, error) {
	if err := renewBoundCookies(a); err != nil {
		return 0, err
	}
	return defaultRotateInterval, nil
}

// renewBoundCookies 主动续一次「设备绑定 cookie」（__Secure-1PSIDTS 一族）。
//
// ★ 为什么必须做这件事（2026-09-12 实测）★
//
//	__Secure-1PSIDTS 是有寿命的票，实测约 10~20 分钟就过期。过期后拿它打
//	https://gemini.google.com/app 会返回「匿名单页」：HTTP 200，但页面里没有
//	SNlM0e —— 上游把这次请求当匿名用户处理。于是 gw2a 判「cookie 失效」、
//	fail_count 累加、账号被自动停用，客户端看到 502「重定向到登录页」。
//	这不是「号坏了」，是「票旧了」，续票就能自愈。
//
// 2026-09-13 起走新版 POST /RotateCookies 端点（见文件头），不再碰已废弃的
// RotateCookiesPage iframe 流程。
func renewBoundCookies(a CookieAccount) error {
	proxyURL := ""
	if a.ProxyID > 0 {
		proxyURL = proxyURLByID(a.ProxyID)
	}
	headers := rotatePostHeaders()
	headers["Cookie"] = a.Cookie
	status, setCookie, respBody, err := rotateDo("POST", rotatePostURL, headers,
		[]byte(rotateTicketBody), proxyURL)
	if err != nil {
		return err
	}
	// 429 = 换得太勤，不是失败：等下一轮就行，不改 cookie、不让健康度背锅。
	if status == 429 {
		logf("[renew] 账号 #%d 续票被限流（429），等下一轮：%s", a.ID, truncate(string(respBody), 90))
		return nil
	}
	if status != 200 {
		return fmt.Errorf("RotateCookies 返回 HTTP %d: %s", status, truncate(string(respBody), 120))
	}
	merged := mergeSetCookie(a.Cookie, setCookie)
	if names := setCookieNames(setCookie); len(names) > 0 {
		logf("[renew] 账号 #%d 续票刷新了 %s", a.ID, strings.Join(names, ", "))
	} else {
		logf("[renew] 账号 #%d 续票 200 但没有 Set-Cookie（响应 %d 字节），按无变化处理", a.ID, len(respBody))
	}
	if merged != a.Cookie {
		updateAccountCookie(a.ID, merged)
	}
	return nil
}

// renewAccountBoundCookies 按 id 续票，返回续票后的新 cookie（失败返回原 cookie）。
func renewAccountBoundCookies(id int64) string {
	a := accountByID(id)
	if a == nil {
		return ""
	}
	if err := renewBoundCookies(*a); err != nil {
		logf("[renew] 账号 #%d 续票失败（忽略，不改健康度）：%v", id, err)
		return a.Cookie
	}
	if fresh := accountByID(id); fresh != nil {
		return fresh.Cookie
	}
	return a.Cookie
}

// rotateAllAccounts 给池子里每个启用的账号做一次保活，返回下次该等多久。
//
// 失败**不计入健康度**：保活打的是 accounts.google.com，跟对话能不能用是两码事，
// 网络抖一下就把号标成坏的，会让它在挑号时沉底，反而伤可用性。
func rotateAllAccounts() time.Duration {
	next := defaultRotateInterval
	for _, a := range accountList() {
		if a.Status != "enabled" {
			continue
		}
		iv, err := rotateAccount(a)
		if err != nil {
			logf("[rotate] 账号 #%d 保活失败: %v", a.ID, err)
			continue
		}
		if iv > 0 {
			next = iv
		}
	}
	return next
}

// setCookieNames 把 Set-Cookie 头里的名字抽出来去重，只用于日志。
func setCookieNames(headers []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range headers {
		name := h
		if i := strings.Index(name, "="); i > 0 {
			name = name[:i]
		}
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// rotatePostHeaders 是 POST /RotateCookies 那一发的完整头，调用方只补 Cookie。
//
// Origin/Referer 必须是 gemini.google.com：这个 POST 在浏览器里是从 Gemini 页面
// 上下文发出的（扩展 forceRotateCookies 实测如此），从 accounts.google.com 的
// Origin 发（旧 iframe 抓包的形状）跟着废弃的页面流程一起失效。
func rotatePostHeaders() map[string]string {
	return map[string]string{
		"Accept":         "*/*",
		"Content-Type":   "application/json",
		"Origin":         "https://gemini.google.com",
		"Referer":        "https://gemini.google.com/",
		"Cache-Control":  "no-cache",
		"Pragma":         "no-cache",
		"Sec-Fetch-Dest": "empty",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Site": "same-site",
	}
}

// rotateDo 走跟正式请求同一个出口：保活从别的 IP 发，等于告诉上游这个会话在两处活动。
//
// 两条传输路径共用同一份 header。以前只有走代理那条调 applyChromeHeaders，直连那条
// 连 User-Agent 都不发 —— 同一个账号在上游看来会因为走没走代理而呈现两种客户端。
func rotateDo(method, url string, headers map[string]string, body []byte, proxyURL string) (
	int, []string, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	merged := map[string]string{
		"User-Agent":         ChromeUA,
		"Accept-Language":    "en-US,en;q=0.9",
		"Sec-CH-UA":          `"Chromium";v="146", "Google Chrome";v="146", "Not?A_Brand";v="24"`,
		"Sec-CH-UA-Mobile":   "?0",
		"Sec-CH-UA-Platform": `"Windows"`,
	}
	for k, v := range headers {
		merged[k] = v
	}
	if proxyURL != "" {
		req, err := http.NewRequest(method, url, rdr)
		if err != nil {
			return 0, nil, nil, err
		}
		for k, v := range merged {
			req.Header.Set(k, v)
		}
		resp, err := getStdlibClient(proxyURL).Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Values("Set-Cookie"), b, err
	}
	req, err := fhttp.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range merged {
		req.Header.Set(k, v)
	}
	resp, err := getTLSClient().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Values("Set-Cookie"), b, err
}
