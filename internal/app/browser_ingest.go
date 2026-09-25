package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 远程浏览器 cookie 接收端点。
//
// 场景（用户要求 2026-09-16）：用户想用**自己电脑上的 Chrome/Edge** 抓取，
// 而不是服务器的 Chromium 容器。链路：
//
//	用户浏览器安装扩展（/admin/api/browser/extension 下载）
//	  → 扩展读本机 gemini.google.com 的 cookie（含登录态判断、失效才刷新页面）
//	  → POST /api/browser/ingest { profile, cookie, logged_in, ... }
//	  → 本服务入库 + 用默认模型校验（与容器路径同一套 browserVerifyAfterFetch）
//
// 安全：走 requireAPIKey（扩展配置里填 API key）。与容器路径的
// /cookie-sync（控制器侧、内网限定）分开 —— 这个端点面向公网可达的部署。

// handleBrowserIngest — POST /api/browser/ingest
//
// CORS：扩展 service worker 从 chrome-extension:// 源发起 POST + Authorization
// 头，浏览器先发 OPTIONS 预检。预检必须**免鉴权**放行并回 CORS 头，否则
// fetch 直接 "Failed to fetch"（2026-09-17 线上实测）。实际 POST 仍走
// requireAPIKey 鉴权。源限定为 chrome-extension:// —— 网页源不允许跨域调用。
func handleBrowserIngest(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if strings.HasPrefix(origin, "chrome-extension://") {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-GW2A-Ext-Mode")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	var p struct {
		Profile   string `json:"profile"`
		Cookie    string `json:"cookie"`
		LoggedIn  *bool  `json:"logged_in"`
		Label     string `json:"label"`
		Note      string `json:"note"`
		UserAgent string `json:"user_agent"`
		Client    string `json:"client"`
		// Account 是扩展上报的 Google 账号槽位（'' / '0' = 默认账号，'1' = /u/1/…）。
		// 多账号共用同一份 cookie，靠 URL 路径切号（见 authuser.go）。
		Account string `json:"account"`
		// Summary 扩展发的是对象（{SID:{len,expires},...}），用 RawMessage
		// 接住再序列化成字符串存 note，避免类型不匹配 400（2026-09-17 实测）。
		Summary json.RawMessage `json:"summary"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	profile := strings.TrimSpace(p.Profile)
	if profile == "" {
		profile = "remote1"
	}
	profile = sanitizeProfileName(profile)
	// 扩展每次回传都带推送模式头；面板据此自动显示/隐藏「在我的浏览器打开
	// 授权页」按钮（service = 用户自己的浏览器，controller = 服务器浏览器）。
	if mode := normalizeExtMode(r.Header.Get("X-GW2A-Ext-Mode")); mode != "" {
		// 从 summary JSON 对象中统计 cookie 数量，避免硬编码 0 覆盖心跳值
		ingestCount := 0
		if len(p.Summary) > 0 && p.Summary[0] == '{' {
			var sm map[string]json.RawMessage
			if json.Unmarshal(p.Summary, &sm) == nil {
				ingestCount = len(sm)
			}
		}
		saveBrowserExtReport(profile, mode, p.LoggedIn != nil && *p.LoggedIn, ingestCount, "cookie 回传")
	}
	cookie := strings.TrimSpace(p.Cookie)
	if cookie == "" {
		writeJSON(w, 400, map[string]string{"error": "cookie 为空"})
		return
	}
	// 扩展上报 logged_in=false 时拒收（页面被 Google 判匿名时 cookie 名字齐全
	// 但服务端不认，写进池子只会制造 302 循环）。
	if p.LoggedIn != nil && !*p.LoggedIn {
		writeJSON(w, 200, map[string]interface{}{
			"ok": false, "detail": "扩展探针判定会话未登录（logged_in=false），已拒收",
		})
		return
	}
	if extractSAPISID(cookie) == "" || !strings.Contains(cookie, "__Secure-1PSID=") {
		writeJSON(w, 400, map[string]string{"error": "cookie 缺 SAPISID 或 __Secure-1PSID，未登录或抓取不完整"})
		return
	}

	label := strings.TrimSpace(p.Label)
	if label == "" {
		label = "remote:" + profile
	}
	note := strings.TrimSpace(p.Note)
	if note == "" {
		note = fmt.Sprintf("远程浏览器扩展推送 @ %s（%s）", time.Now().Format("2006-01-02 15:04:05"), truncate(p.Client, 40))
	}
	// summary 是扩展上报的 cookie 摘要对象，原样序列化进 note 备查
	if len(p.Summary) > 0 && string(p.Summary) != "null" {
		note += " · summary=" + truncate(string(p.Summary), 400)
	}

	// 账号槽位：优先用扩展显式上报的 account，没带就从 profile 名推断
	// （扩展把多账号落成 browser1 / browser1-u1 / browser1-u2…）。
	authUser := parseAuthUser(p.Account)
	if authUser == 0 {
		authUser = authUserFromProfile(profile)
	}

	// 入库（与容器路径同一套存储逻辑，source 标 remote 以示区分）
	if err := browserStoreCookieSourceAuthUser(label, profile, cookie, "remote", authUser); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	logf("[ingest] 远程 profile %q cookie 已入库（%d 字节, 槽位 %s, client=%s）",
		profile, len(cookie), authUserLabel(authUser), truncate(p.Client, 30))

	// 抓取后模型校验（与容器路径一致：502 重抓提示 / 302 重置代理池）
	vr := browserVerifyAfterFetch(label, profile, false)

	resp := map[string]interface{}{
		"ok":     vr.OK,
		"detail": "cookie 已入库并完成模型校验",
		"verify": vr,
	}
	if !vr.OK {
		resp["detail"] = "cookie 已入库，但默认模型校验未通过：" + vr.Detail
	}
	writeJSON(w, 200, resp)
}

// sanitizeProfileName 把 profile 名收敛到安全字符集（与控制器侧规则一致）。
func sanitizeProfileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	out := b.String()
	if out == "" {
		out = "remote1"
	}
	return out
}
