package app

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// Gemini 在带登录 cookie 时要求 batchexecute 请求多带一个表单字段 at（XSRF token），
// 不带就直接 400，响应体形如 [["er",...,400,...,[{"48448350":["xsrf", ...]}]]]。
// 匿名请求不需要它，所以这个坑一直没暴露——一挂上有效 cookie，所有请求立刻全挂。
//
// token 来自 /app 页面 HTML 里的 "SNlM0e":"<token>:<毫秒时间戳>"，跟 cookie 会话
// 绑定，所以按 cookie 分别缓存。实测同一 token 可以复用，过期后服务端还是回 xsrf
// 错误，调用方拿到这个错要 invalidate 再取一次。
var (
	xsrfMu    sync.Mutex
	xsrfCache = map[string]xsrfEntry{}
)

type xsrfEntry struct {
	token   string
	pushID  string // 上传文件用的 Push-ID 头
	pctx    string // 上传文件用的 X-Client-Pctx 头
	fetched time.Time
}

// 页面上的 token 没写明有效期，取个保守值定期重取。
const xsrfTTL = 20 * time.Minute

var snlm0eRe = regexp.MustCompile(`"SNlM0e":"([^"]{10,200})"`)

// 上传要的两个页面参数，跟 XSRF token 同页取，省一次页面请求。
var pushIDRe = regexp.MustCompile(`"qKIAYe":"([^"]{4,400})"`)

var pctxRe = regexp.MustCompile(`"Ylro7b":"([^"]{4,400})"`)

// cookieKey 用 cookie 的短摘要当缓存键，避免把整串凭证塞进 map key。
func cookieKey(cookie string) string {
	sum := sha1.Sum([]byte(cookie))
	return hex.EncodeToString(sum[:8])
}

// invalidateXSRF 丢掉某个 cookie 的缓存 token，下次取会重新抓页面。
func invalidateXSRF(cookie string) {
	if cookie == "" {
		return
	}
	xsrfMu.Lock()
	delete(xsrfCache, cookieKey(cookie))
	xsrfMu.Unlock()
}

// getXSRF 取该 cookie 对应的 XSRF token；命中缓存且没过期就直接返回。
// cookie 为空（匿名）时返回空串——匿名请求不需要这个字段。
func getXSRF(cookie, proxyURL string) (string, error) {
	if cookie == "" {
		return "", nil
	}
	key := cookieKey(cookie)

	xsrfMu.Lock()
	if e, ok := xsrfCache[key]; ok && time.Since(e.fetched) < xsrfTTL {
		xsrfMu.Unlock()
		return e.token, nil
	}
	xsrfMu.Unlock()

	e, err := fetchAppTokens(cookie, proxyURL)
	if err != nil {
		return "", err
	}
	xsrfMu.Lock()
	xsrfCache[key] = e
	xsrfMu.Unlock()
	return e.token, nil
}

// getUploadTokens 取上传要用的 Push-ID / X-Client-Pctx，跟 XSRF token 同一份缓存。
func getUploadTokens(cookie, proxyURL string) (pushID, pctx string, err error) {
	key := cookieKey(cookie)

	xsrfMu.Lock()
	if e, ok := xsrfCache[key]; ok && time.Since(e.fetched) < xsrfTTL {
		xsrfMu.Unlock()
		return e.pushID, e.pctx, nil
	}
	xsrfMu.Unlock()

	e, err := fetchAppTokens(cookie, proxyURL)
	if err != nil {
		return "", "", err
	}
	xsrfMu.Lock()
	xsrfCache[key] = e
	xsrfMu.Unlock()
	return e.pushID, e.pctx, nil
}

// httpStatusErr 把非 200 响应连 Location 一起带出来。
//
// 为什么必须带 Location：Google 有**两种**完全不同的非 200——
//
//	· 302 -> accounts.google.com/ServiceLogin   = cookie 真的失效了
//	· 302 -> www.google.com/sorry/index         = 出口 IP 被反爬，cookie 是好的，
//	                                             过几分钟封禁自己会过期
//
// 不带 Location 就没法区分，只能一律当成 cookie 失效，
// 于是好号被反复判死、fail_count 累加、最后被自动停用（2026-09-12 实测踩到）。
func httpStatusErr(prefix string, resp *http.Response) error {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return fmt.Errorf("%s: HTTP %d", prefix, resp.StatusCode)
	}
	return fmt.Errorf("%s: HTTP %d -> %s", prefix, resp.StatusCode, loc)
}

// httpStatusErrF 同上，只是给 fhttp（tls-client 那套）用 —— 它的 Response
// 是另一个包的类型，没法跟 net/http.Response 共用一个函数。
func httpStatusErrF(prefix string, resp *fhttp.Response) error {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return fmt.Errorf("%s: HTTP %d", prefix, resp.StatusCode)
	}
	return fmt.Errorf("%s: HTTP %d -> %s", prefix, resp.StatusCode, loc)
}

// fetchAppPage 抓 gemini.google.com/app 的 HTML。
// 走跟主请求相同的出口：配了代理走 stdlib，没配走 tls-client，
// 免得页面里取到的 token 和后续请求来自两个不同 IP。
// cookie 传空串就是匿名抓（页面照样返回，只是没有登录态字段）。
func fetchAppPage(cookie, proxyURL string) ([]byte, error) {
	const pageURL = "https://gemini.google.com/app"
	headers := map[string]string{
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-US,en;q=0.9",
	}
	if cookie != "" {
		headers["Cookie"] = cookie
	}

	var body []byte
	if proxyURL != "" {
		req, err := http.NewRequest("GET", pageURL, nil)
		if err != nil {
			return nil, err
		}
		applyChromeHeaders(req)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := getStdlibClient(proxyURL).Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, httpStatusErr("fetch /app", resp)
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
	} else {
		req, err := fhttp.NewRequest("GET", pageURL, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := getTLSClient().Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, httpStatusErrF("fetch /app", resp)
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// fetchAppTokens 抓一次 /app 页面，把三个 token 一起抠出来。
func fetchAppTokens(cookie, proxyURL string) (xsrfEntry, error) {
	body, err := fetchAppPage(cookie, proxyURL)
	if err != nil {
		return xsrfEntry{}, err
	}
	e := xsrfEntry{fetched: time.Now()}
	if m := snlm0eRe.FindSubmatch(body); m != nil {
		e.token = string(m[1])
	} else if cookie != "" {
		// 带 cookie 却拿不到 token = cookie 已失效被当成匿名。匿名本就没这字段，不算错。
		return xsrfEntry{}, fmt.Errorf("no SNlM0e in page (cookie expired or not signed in)")
	}
	if p := pushIDRe.FindSubmatch(body); p != nil {
		e.pushID = string(p[1])
	}
	if p := pctxRe.FindSubmatch(body); p != nil {
		e.pctx = string(p[1])
	}
	return e, nil
}

// isXSRFError 判断上游 400 是不是 XSRF token 的问题。
// 响应体形如：[["er",null,...,400,...,[{"48448350":["xsrf","<新token>",...]}]]]
func isXSRFError(raw string) bool {
	return strings.Contains(raw, `"xsrf"`)
}

// xsrfAuthStatus 把 getXSRF 的错误映射成 cookie 健康度用的状态码。
//
// 只有**确实说明 cookie 失效**的错误才返回 401：
//   - "no SNlM0e in page (cookie expired or not signed in)"：页面打开了但里面没有 token
//   - "fetch /app: HTTP 401/403"：上游明确告诉我们鉴权失败
//
// EOF、超时、代理挂掉、502/503、DNS 失败、302 一律返回 0，
// 交给 markCookieByStatus 的 default 分支忽略。
//
// 为什么要这么严？因为一旦断网，每次请求都会累加 fail_count，
// 3 次就会把好号自动停用，甚至被清理出池子。
// 历史上真发生过。
func xsrfAuthStatus(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	const pfx = "fetch /app: HTTP "
	if strings.HasPrefix(msg, pfx) {
		rest := strings.TrimSpace(msg[len(pfx):])
		// 状态码后面现在可能跟着 " -> <location>"（见 httpStatusErr），先切掉
		code := rest
		if i := strings.Index(rest, " "); i > 0 {
			code = rest[:i]
		}
		if code == "401" || code == "403" {
			return 401
		}
		// 302（含 sorry / consent 跳转）、429、5xx 都算网络或反爬类，
		// 绝不记进 cookie 健康度
		return 0
	}
	// 「页面里没有 SNlM0e」**不再**算 cookie 鉴权失败。
	//
	// 历史判据把它当 401，于是累加 fail_count、3 次自动停用账号。但 2026-09-12
	// 实测证明这个症状绝大多数只是 __Secure-1PSIDTS 这张票过期（约 10~20 分钟），
	// 带着过期票打 /app 一律拿到「匿名单页」；续票或浏览器刷新后同一份 cookie
	// 立刻恢复。把它判死，就会让「web 明明登录着」变成永久 502 —— 用户报的正是这个。
	// 真失效由更外层的探针负责（会话看门狗 / 重新登录工具）。
	if strings.Contains(msg, "no SNlM0e in page") {
		return 0
	}
	return 0
}
