package app

import (
	"strconv"
	"strings"
)

// Google 多账号槽位（URL 里的 /u/N/）。
//
// ★ 2026-09-24 实测（本项目「固定抓取页」失效的根因）★
//
// 同一个浏览器 profile 里同时登录多个 Google 账号时，**cookie 是共享的**：
// 分别打开 /app 与 /u/1/app 两页读 cookie，SID / HSID / SSID / APISID /
// SAPISID / __Secure-1PSID / __Secure-3PSID / LSID / NID / COMPASS 等
// 逐项 sha1 完全相同（实测两份 22KB 的串，唯一差异是 __Secure-1PSIDTS 和
// SIDCC 这两张短命轮换票）。
//
// 决定「这次请求算哪个账号」的是 **URL 里的 /u/N/ 路径**，不是 cookie：
//
//	https://gemini.google.com/app      -> autumnsentiment@gmail.com（账号 #1）
//	https://gemini.google.com/u/1/app  -> panfudi9@gmail.com        （账号 #2）
//
// 同一份 cookie 打这两个地址拿到的是**两个不同的账号**。所以「固定了第二个
// 账号的页面，抓回来还是第一个账号」的根因不在扩展：扩展抓的 cookie 本来就
// 是同一份，而服务端把上游 URL 和 X-Goog-AuthUser 都写死成了默认账号。
//
// 还有一条实测约束：XSRF token（页面 HTML 里的 SNlM0e）**跟槽位绑定** ——
// 拿 /app 页面的 token 去打 /u/1/ 端点，上游回 400；反之亦然。所以取 token
// 的那次页面请求（fetchAppPage）必须跟正式请求用同一个槽位。
//
// 匿名请求没有账号概念，槽位恒为 0。

// geminiURLPrefix 返回槽位对应的 URL 路径前缀：默认账号空串，N 号账号 "/u/N"。
// 用法：base + geminiURLPrefix(n) + "/_/BardChatUi/data/…"
func geminiURLPrefix(authuser int) string {
	if authuser <= 0 {
		return ""
	}
	return "/u/" + strconv.Itoa(authuser)
}

// geminiPageURL 返回槽位对应的 Gemini 首页地址，取 XSRF token 用。
func geminiPageURL(authuser int) string {
	return "https://gemini.google.com" + geminiURLPrefix(authuser) + "/app"
}

// geminiRootURL 返回槽位对应的站点根地址（上传等请求的 Referer 用，浏览器发的是根路径）。
func geminiRootURL(authuser int) string {
	return "https://gemini.google.com" + geminiURLPrefix(authuser) + "/"
}

// authUserHeaderValue 把 X-Goog-AuthUser 头设成槽位值（默认 0）。
// 上游实测不靠这个头切号（只看 URL 路径），但浏览器两个都发，保持一致更安全。
func authUserHeaderValue(authuser int) string {
	if authuser <= 0 {
		return "0"
	}
	return strconv.Itoa(authuser)
}

// parseAuthUser 解析扩展上报的账号槽位字符串（'' / '0' -> 0，非法 -> 0）。
func parseAuthUser(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 9 {
		return 0
	}
	return n
}

// authUserFromProfile 从 profile 名推断槽位。
//
// 扩展把多账号落成不同 profile（browser1 / browser1-u1 / browser1-u2…），
// 后缀 -uN 就是槽位。老配置没有后缀 -> 默认账号 0。
func authUserFromProfile(profile string) int {
	i := strings.LastIndex(profile, "-u")
	if i < 0 {
		return 0
	}
	return parseAuthUser(profile[i+2:])
}

// authUserLabel 是面板/日志里给人看的槽位说明。
func authUserLabel(authuser int) string {
	if authuser <= 0 {
		return "默认账号"
	}
	return "账号槽位 " + strconv.Itoa(authuser)
}
