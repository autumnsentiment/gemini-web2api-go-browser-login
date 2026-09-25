package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	extModeKeyPrefix        = "ext_mode:"
	extSeenKeyPrefix        = "ext_seen_at:"
	extDetailKeyPrefix      = "ext_last_detail:"
	extLoggedInKeyPrefix    = "ext_logged_in:"
	extCookieCountKeyPrefix = "ext_cookie_count:"

	// extOnlineTTL 是扩展主动上报心跳后的在线判定窗口。
	// 心跳默认 5 分钟，留一倍余量避免刚好到点时被误判为休眠。
	extOnlineTTL = 10 * time.Minute
)

// browserExtReport 是面板展示的扩展状态。
type browserExtReport struct {
	Profile     string `json:"profile"`
	Mode        string `json:"mode"`
	State       string `json:"state"`
	LoggedIn    bool   `json:"logged_in"`
	CookieCount int    `json:"cookie_count"`
	LastSeenAt  int64  `json:"last_seen_at"`
	LastDetail  string `json:"last_detail"`
}

func browserExtModeKey(profile string) string        { return extModeKeyPrefix + profile }
func browserExtSeenKey(profile string) string        { return extSeenKeyPrefix + profile }
func browserExtDetailKey(profile string) string      { return extDetailKeyPrefix + profile }
func browserExtLoggedInKey(profile string) string    { return extLoggedInKeyPrefix + profile }
func browserExtCookieCountKey(profile string) string { return extCookieCountKeyPrefix + profile }

// normalizeExtMode 把扩展回传的模式头/字段统一成 service / controller。
func normalizeExtMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "service", "server", "remote":
		return "service"
	case "controller", "local", "container", "browser":
		return "controller"
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

func browserExtStatus(profile string) browserExtReport {
	seen, _ := strconv.ParseInt(kvGet(browserExtSeenKey(profile)), 10, 64)
	state := "sleeping"
	if seen > 0 && time.Since(time.Unix(seen, 0)) <= extOnlineTTL {
		state = "online"
	}
	count, _ := strconv.Atoi(kvGet(browserExtCookieCountKey(profile)))
	return browserExtReport{
		Profile:     profile,
		Mode:        normalizeExtMode(kvGet(browserExtModeKey(profile))),
		State:       state,
		LoggedIn:    kvGet(browserExtLoggedInKey(profile)) == "1",
		CookieCount: count,
		LastSeenAt:  seen,
		LastDetail:  kvGet(browserExtDetailKey(profile)),
	}
}

// saveBrowserExtReport 写入扩展主动回传的状态。mode 为空时保留旧值。
func saveBrowserExtReport(profile, mode string, loggedIn bool, cookieCount int, detail string) {
	if mode == "" {
		mode = normalizeExtMode(kvGet(browserExtModeKey(profile)))
	}
	_ = kvSet(browserExtModeKey(profile), normalizeExtMode(mode))
	_ = kvSet(browserExtSeenKey(profile), strconv.FormatInt(time.Now().Unix(), 10))
	_ = kvSet(browserExtLoggedInKey(profile), map[bool]string{true: "1", false: "0"}[loggedIn])
	_ = kvSet(browserExtCookieCountKey(profile), strconv.Itoa(cookieCount))
	if detail != "" {
		_ = kvSet(browserExtDetailKey(profile), truncate(detail, 300))
	}
}

// browserExtCORS 处理 chrome-extension:// 源和 OPTIONS 预检，返回是否已终结。
func browserExtCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if strings.HasPrefix(origin, "chrome-extension://") {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-GW2A-Ext-Mode")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// handleBrowserExtensionStatus — POST /api/browser/extension-status
//
// 扩展主动上报存活/模式用。service 模式直连本服务；controller 模式经控制器
// pool.py 写同一份 kv。模式头 X-GW2A-Ext-Mode 优先，body.push_mode 兜底。
func handleBrowserExtensionStatus(w http.ResponseWriter, r *http.Request) {
	if browserExtCORS(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	var p struct {
		Profile     string `json:"profile"`
		PushMode    string `json:"push_mode"`
		LoggedIn    *bool  `json:"logged_in"`
		CookieCount int    `json:"cookie_count"`
		Detail      string `json:"detail"`
		Client      string `json:"client"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	profile := sanitizeProfileName(p.Profile)
	if profile == "" {
		writeJSON(w, 400, map[string]string{"error": "profile 为空"})
		return
	}
	mode := normalizeExtMode(r.Header.Get("X-GW2A-Ext-Mode"))
	if mode == "" {
		mode = normalizeExtMode(p.PushMode)
	}
	logged := p.LoggedIn != nil && *p.LoggedIn
	saveBrowserExtReport(profile, mode, logged, p.CookieCount, p.Detail)
	logf("[ext-status] profile=%s mode=%s logged_in=%v cookie_count=%d", profile, mode, logged, p.CookieCount)
	writeJSON(w, 200, map[string]interface{}{"ok": true, "profile": profile, "mode": mode})
}

// handleAdminBrowserExtensionSync — POST /admin/api/browser/extension-sync
//
// 面板「同步插件状态」按钮：对每个 profile 探测扩展是否存活。controller 模式
// 走 CDP 主动探测；service 模式以最近心跳为准，无法反向连用户浏览器。
func handleAdminBrowserExtensionSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	names := browserExtProfileNames()
	items := make([]browserExtReport, 0, len(names))
	for _, name := range names {
		items = append(items, browserExtSyncOne(name))
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "items": items})
}

func browserExtProfileNames() []string {
	set := map[string]bool{}
	for _, a := range browserAccounts() {
		if a.Profile != "" {
			set[a.Profile] = true
		}
	}
	if browserEnabled() {
		var out struct {
			Profiles []struct {
				Name string `json:"name"`
			} `json:"profiles"`
		}
		code, err := browserHTTP(http.MethodGet, browserControllerURL()+"/profiles", nil, &out)
		if err == nil && code == 200 {
			for _, p := range out.Profiles {
				if p.Name != "" {
					set[p.Name] = true
				}
			}
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func browserExtSyncOne(profile string) browserExtReport {
	rep := browserExtStatus(profile)
	if rep.Mode == "service" {
		if rep.LastSeenAt == 0 {
			rep.State = "sleeping"
			rep.LastDetail = "扩展尚未向服务端上报状态，视为休眠"
		} else if rep.State == "sleeping" {
			rep.LastDetail = "扩展最近上报已超时，视为休眠"
		} else {
			rep.LastDetail = "扩展在线（服务端模式心跳）"
		}
		return rep
	}
	if !browserEnabled() {
		rep.State = "sleeping"
		rep.LastDetail = "未启用浏览器控制器，扩展不可达"
		return rep
	}
	port := browserRunningProfilePort(profile)
	if port <= 0 {
		rep.State = "sleeping"
		rep.LastDetail = "浏览器未运行（已休眠）"
		return rep
	}
	ok, found, detail := browserProbeExtension(profile, port)
	if found && ok {
		after := browserExtStatus(profile)
		after.State = "online"
		after.LastDetail = detail
		return after
	}
	rep.State = "sleeping"
	rep.LastDetail = detail
	return rep
}

// browserRunningProfilePort 只读查询控制器，不唤醒休眠中的 profile。
func browserRunningProfilePort(profile string) int {
	if !browserEnabled() {
		return 0
	}
	var out struct {
		Profiles []struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		} `json:"profiles"`
	}
	code, err := browserHTTP(http.MethodGet, browserControllerURL()+"/profiles", nil, &out)
	if err != nil || code != 200 {
		return 0
	}
	for _, p := range out.Profiles {
		if p.Name == profile {
			return p.Port
		}
	}
	return 0
}

// browserControllerPort 解析控制器 URL 的端口，供浏览器桥接页访问。
func browserControllerPort() string {
	u := browserControllerURL()
	if i := strings.LastIndex(u, ":"); i >= 0 && i+1 < len(u) {
		rest := u[i+1:]
		if j := strings.IndexAny(rest, "/"); j >= 0 {
			rest = rest[:j]
		}
		if p, err := strconv.Atoi(rest); err == nil && p > 0 {
			return strconv.Itoa(p)
		}
	}
	return "9280"
}

// browserProbeExtension 在控制器托管的桥接页里向扩展发 status 消息。
// found=false 表示 profile 没跑起来或控制器不可达；found=true 但 ok=false
// 表示浏览器在跑，但扩展没安装/没响应。
func browserProbeExtension(profile string, port int) (ok, found bool, detail string) {
	var idOut struct {
		ExtID string `json:"ext_id"`
	}
	code, err := browserHTTP(http.MethodGet, browserControllerURL()+"/extension-id", nil, &idOut)
	if err != nil || code != 200 || strings.TrimSpace(idOut.ExtID) == "" {
		return false, false, "控制器未返回扩展 ID"
	}
	hostPort := fmt.Sprintf("%s:%d", browserCDPHost(), port)
	bridgeURL := "http://127.0.0.1:" + browserControllerPort() + "/extension"
	ws, err := cdpOpenTab(hostPort, bridgeURL)
	if err != nil {
		return false, true, "打开扩展桥接页失败: " + err.Error()
	}
	defer closeCDPTab(hostPort, ws)

	ready := false
	for i := 0; i < 40; i++ {
		out, evalErr := cdpEvalWS(ws, `location.href+'|'+document.readyState`)
		if evalErr == nil {
			s := strings.TrimSpace(out)
			if strings.Contains(s, "/extension") && strings.Contains(s, "|complete") {
				ready = true
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ready {
		return false, true, "扩展桥接页未加载完成"
	}

	extJSON, _ := json.Marshal(idOut.ExtID)
	expr := `new Promise((resolve) => {
		try {
			chrome.runtime.sendMessage(` + string(extJSON) + `, {type:'status'}, (resp) => {
				resolve(JSON.stringify({
					err: chrome.runtime.lastError ? chrome.runtime.lastError.message : null,
					resp: resp || null
				}));
			});
		} catch (e) { resolve(JSON.stringify({err: String(e)})); }
	})`
	out, err := cdpEvalWS(ws, expr)
	if err != nil {
		return false, true, "扩展状态查询失败: " + err.Error()
	}
	var probe struct {
		Err  string `json:"err"`
		Resp *struct {
			OK          bool `json:"ok"`
			LoggedIn    bool `json:"logged_in"`
			CookieCount int  `json:"cookie_count"`
		} `json:"resp"`
	}
	if err := json.Unmarshal([]byte(out), &probe); err != nil || probe.Resp == nil {
		return false, true, "扩展未响应（未安装或不可达）"
	}
	if probe.Err != "" || !probe.Resp.OK {
		return false, true, "扩展未响应: " + probe.Err
	}
	saveBrowserExtReport(profile, "controller", probe.Resp.LoggedIn, probe.Resp.CookieCount, "扩展在线（CDP 探测）")
	return true, true, "扩展在线（CDP 探测）"
}

// cdpEvalWS 在指定 WebSocket 页面上下文执行表达式（不需要先选页）。
func cdpEvalWS(wsURL, expr string) (string, error) {
	pg, err := cdpDial(wsURL, 20*time.Second)
	if err != nil {
		return "", err
	}
	defer pg.Close()
	res, err := pg.call("Runtime.evaluate", map[string]interface{}{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return "", err
	}
	var v struct {
		Result struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"result"`
	}
	_ = json.Unmarshal(res, &v)
	return v.Result.Value, nil
}

func closeCDPTab(hostPort, wsURL string) {
	port, err := cdpPortOfHostPort(hostPort)
	if err != nil {
		return
	}
	parts := strings.Split(strings.TrimRight(wsURL, "/"), "/")
	id := parts[len(parts)-1]
	if id == "" {
		return
	}
	resp, err := http.Get(cdpTunnelBase(port) + "/json/close/" + url.PathEscape(id))
	if err == nil {
		resp.Body.Close()
	}
}
