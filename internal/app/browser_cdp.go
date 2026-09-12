package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bogdanfinn/websocket"
)

// ─────────────────────────────────────────────────────────────────────────────
// 浏览器登录（Chromium CDP）支持
//
// 目标：像 9router 那样在本地用浏览器登录 Gemini 网页会话，登录态自动进
// cookie 池，账号之间用**独立 Chromium profile** 隔离。因为本服务跑在
// distroless 精简镜像里（没有 Chromium / 浏览器），浏览器装在旁边的
// chromium(VNC) 容器里，通过一个轻量控制器(controller.js, 监听 9280)管理
// 每个账号的独立 Chromium 实例 + 独立 CDP 调试端口。
//
// 数据流：
//   - 面板「浏览器登录」添加 profile → 控制器在 chromium 容器里拉起
//     `chromium --user-data-dir=... --remote-debugging-port=<port>` 独立实例
//   - 用户在 VNC 桌面(容器 :3000/3001)上登录 Google 账号（该 profile 专属）
//   - 面板点「抓取」/ 后台定时 → 本进程连 CDP WebSocket，导航到
//     gemini.google.com，用 Network.getAllCookies 拿回明文 cookie，
//     拼成 header string 写入 cookie 池（带 source='browser' 标记）
//   - 每 10 分钟自动刷新一次；登录态丢了就自动删除对应池记录
//
// 本文件只做 CDP 客户端；进程/端口生命周期交给 controller.js。
// ─────────────────────────────────────────────────────────────────────────────

// BrowserAccount 是 accounts 表里一条带浏览器 profile 来源的记录（cookie 池行的
// 扩展视图，只读，不脱敏 cookie 给管理端）。
type BrowserAccount struct {
	CookieAccount
	Profile string `json:"profile"`
	Source  string `json:"source"`
}

// ── 配置（来自 cfg，面板「设置」也能改运行时部分）──────────────────────────────
// BrowserControllerURL: controller.js 的 HTTP 基址，例如 http://chromium:9280
// BrowserCDPBase:       CDP 调试端口基址，与 controller 的 GW2A_CDP_BASE 对应
// BrowserProfilesDir:   (仅展示用) profile 目录
var (
	browserMu      sync.Mutex
	browserLastErr string // 上次控制器/抓取错误，供状态页展示
)

// errBrowserPageUnreachable：浏览器页面根本没打开（网络/代理/CDP 问题）。
// 上层看到它只记日志、绝不删号——这不是 cookie 的错。
var errBrowserPageUnreachable = errors.New("浏览器页面打不开（网络/代理不可达）")

// errBrowserNotLoggedIn：页面正常打开，但确实没有登录态。
var errBrowserNotLoggedIn = errors.New("profile 未登录 gemini（页面里没有 SNlM0e）")

func browserEnabled() bool {
	return strings.TrimSpace(cfg.BrowserControllerURL) != ""
}

// browserControllerURL 返回控制器基址（去掉尾部 /）。
func browserControllerURL() string {
	return strings.TrimRight(strings.TrimSpace(cfg.BrowserControllerURL), "/")
}

// ── 轻量 HTTP 帮助 ──────────────────────────────────────────────────────────

func browserHTTP(method, url string, body []byte, out interface{}) (int, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	var rd *strings.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// browserHealth 探测控制器是否在线，返回 (在线?, 运行中 profile 数, 错误串)。
func browserHealth() (bool, int, string) {
	if !browserEnabled() {
		return false, 0, "未启用（未配置 BROWSER_CONTROLLER_URL）"
	}
	var out struct {
		OK       bool `json:"ok"`
		Profiles int  `json:"profiles"`
		Port     int  `json:"port"`
	}
	code, err := browserHTTP(http.MethodGet, browserControllerURL()+"/healthz", nil, &out)
	if err != nil {
		return false, 0, err.Error()
	}
	if code != 200 {
		return false, 0, fmt.Sprintf("控制器 HTTP %d", code)
	}
	return out.OK, out.Profiles, ""
}

// ensureBrowserProfile 让控制器保证某个 profile 的 Chromium 实例在跑，返回 CDP 端口。
func ensureBrowserProfile(name string) (int, error) {
	if !browserEnabled() {
		return 0, fmt.Errorf("浏览器登录未启用：请配置 BROWSER_CONTROLLER_URL")
	}
	var out struct {
		Name string `json:"name"`
		Port int    `json:"port"`
	}
	body, _ := json.Marshal(map[string]string{"name": name})
	code, err := browserHTTP(http.MethodPost, browserControllerURL()+"/profiles", body, &out)
	if err != nil {
		return 0, err
	}
	if code != 200 {
		return 0, fmt.Errorf("控制器返回 HTTP %d", code)
	}
	if out.Port <= 0 {
		return 0, fmt.Errorf("控制器未返回 CDP 端口")
	}
	return out.Port, nil
}

// browserStopProfile 让控制器关掉某个 profile 的实例。
func browserStopProfile(name string) error {
	if !browserEnabled() {
		return nil
	}
	code, err := browserHTTP(http.MethodDelete, browserControllerURL()+"/profiles/"+name, nil, nil)
	if err != nil {
		return err
	}
	if code != 200 && code != 404 {
		return fmt.Errorf("控制器返回 HTTP %d", code)
	}
	return nil
}

// ── CDP WebSocket 客户端 ────────────────────────────────────────────────────

// cdpCall 是一次 CDP 命令：连接页面 WebSocket → 发 {id,method,params} → 等
// 同 id 的响应返回。每次调用独立连接，用完即关（简单可靠，抓 cookie 低频）。
type cdpPage struct {
	wsURL string
	conn  *websocket.Conn
	mu    sync.Mutex
	next  int64
}

func cdpDial(wsURL string, timeout time.Duration) (*cdpPage, error) {
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	// CDP 可能先推送事件，把它们读掉直到读到我们 id 的响应；这里用按 id 匹配。
	return &cdpPage{wsURL: wsURL, conn: conn}, nil
}

func (c *cdpPage) Close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

type cdpResp struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call 发送一条命令并等待其响应。events 用于收集过程中到达的事件（如
// Network.* 通知），这里我们不需要事件，直接等 id 匹配。
func (c *cdpPage) call(method string, params map[string]interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	msg := map[string]interface{}{
		"id": id, "method": method, "params": params,
	}
	b, _ := json.Marshal(msg)
	if err := c.conn.WriteMessage(websocket.TextMessage, b); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	// 读消息直到拿到匹配 id
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.mu.Unlock()
			return nil, err
		}
		var resp cdpResp
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		if resp.ID != id {
			continue
		}
		c.mu.Unlock()
		if resp.Error != nil {
			return nil, fmt.Errorf("CDP %s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// cdpTunnelBase 返回经控制器隧道访问某 CDP 端口的 HTTP 基址。
// 例：BROWSER_CONTROLLER_URL=http://chromium:9280、port=9300 →
//
//	http://chromium:9280/cdp/9300
func cdpTunnelBase(port int) string {
	return fmt.Sprintf("%s/cdp/%d", browserControllerURL(), port)
}

// cdpControllerHostPort 返回控制器的主机:端口（浏览器控制器所在容器）。
func cdpControllerHostPort() string {
	u := browserControllerURL()
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexAny(rest, "/"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return "chromium:9280"
}

// cdpControllerHost 返回控制器的主机名（浏览器控制器所在容器）。
func cdpControllerHost() string {
	hp := cdpControllerHostPort()
	if i := strings.Index(hp, ":"); i >= 0 {
		return hp[:i]
	}
	return hp
}

// cdpPortOfHostPort 从 "chromium:9300" 这类 host:port 里解析出端口。
func cdpPortOfHostPort(hostPort string) (int, error) {
	idx := strings.LastIndex(hostPort, ":")
	if idx < 0 {
		return 0, fmt.Errorf("bad hostPort %q", hostPort)
	}
	port, err := strconv.Atoi(hostPort[idx+1:])
	if err != nil || port <= 0 {
		return 0, fmt.Errorf("bad port in hostPort %q", hostPort)
	}
	return port, nil
}

// cdpRewriteWS 把 Chromium 返回的、绑定在 127.0.0.1 的调试 WS 地址改写成经
// 控制器隧道可达的地址：ws://<controller>/cdp/<port>/devtools/page/<id>
func cdpRewriteWS(raw string, port int) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			path := rest[j:]
			return "ws://" + cdpControllerHostPort() + "/cdp/" + strconv.Itoa(port) + path
		}
	}
	return raw
}

// cdpListPages 取 CDP 端口上的 page 目标，返回第一个 page 的（隧道）ws url。
func cdpListPages(hostPort string) (string, error) {
	port, err := cdpPortOfHostPort(hostPort)
	if err != nil {
		return "", err
	}
	httpURL := cdpTunnelBase(port) + "/json/list"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(httpURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var list []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
		WS   string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return "", err
	}
	for _, t := range list {
		if t.Type == "page" && t.WS != "" {
			return cdpRewriteWS(t.WS, port), nil
		}
	}
	return "", fmt.Errorf("CDP 端口上没有 page 目标")
}

// cdpOpenTab 开一个新 tab 并导航，返回该 page 的（隧道）ws url。
// 用 HTTP /json/new?url 接口（PUT）。
func cdpOpenTab(hostPort, url string) (string, error) {
	port, err := cdpPortOfHostPort(hostPort)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	newURL := cdpTunnelBase(port) + "/json/new?" + urlQueryEscape(url)
	req, _ := http.NewRequest(http.MethodPut, newURL, nil)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.WS == "" {
		return "", fmt.Errorf("新建 tab 未返回 ws url")
	}
	return cdpRewriteWS(out.WS, port), nil
}

func urlQueryEscape(s string) string {
	// 简单编码：只处理空格与特殊字符，够用即可
	replacer := strings.NewReplacer(" ", "%20", "\"", "%22", "'", "%27", "<", "%3C", ">", "%3E")
	return replacer.Replace(s)
}

// cdpNavigate 导航到 url（若无 page 就开新 tab），等待基本加载。
func cdpNavigate(hostPort, url string) error {
	ws, err := cdpListPages(hostPort)
	if err != nil {
		// 没有 page 就开一个
		ws, err = cdpOpenTab(hostPort, url)
		if err != nil {
			return err
		}
	}
	pg, err := cdpDial(ws, 15*time.Second)
	if err != nil {
		return err
	}
	defer pg.Close()
	_, err = pg.call("Page.navigate", map[string]interface{}{"url": url})
	if err != nil {
		return err
	}
	// 等几秒让页面加载（登录态判断在后续 getAllCookies/SNlM0e 单独做）
	time.Sleep(3 * time.Second)
	return nil
}

// cdpGetCookies 通过 CDP 抓取 host 相关域的 cookie，拼成 "k=v; k=v" header 串。
// 只保留登录态需要的域：gemini.google.com 及 .google.com 的会话 cookie。
func cdpGetCookies(hostPort string) (string, error) {
	ws, err := cdpListPages(hostPort)
	if err != nil {
		return "", err
	}
	pg, err := cdpDial(ws, 15*time.Second)
	if err != nil {
		return "", err
	}
	defer pg.Close()

	// Network.enable 让 cookie 域信息返回
	_, _ = pg.call("Network.enable", map[string]interface{}{})
	res, err := pg.call("Network.getAllCookies", map[string]interface{}{})
	if err != nil {
		return "", err
	}
	var data struct {
		Cookies []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Domain string `json:"domain"`
			Path   string `json:"path"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(res, &data); err != nil {
		return "", err
	}
	// 同一名字可能出现在多个域（例如 SAPISID 同时挂在 .google.com 与
	// gemini.google.com）。浏览器发给 gemini.google.com 的 Cookie 头每个名字
	// 只带一个值 —— 域越宽优先级越高（.google.com 上的值才是被 Gemini 用于
	// 授权的那个）。所以收集时按「域宽度」保留优先级最高的那份。
	type ckItem struct {
		name, val, domain string
		width             int
	}
	var items []ckItem
	for _, ck := range data.Cookies {
		d := strings.ToLower(strings.TrimPrefix(ck.Domain, "."))
		if !(d == "google.com" || d == "gemini.google.com" ||
			strings.HasSuffix(d, ".google.com") || strings.HasSuffix(d, ".gemini.google.com")) {
			continue
		}
		if ck.Name == "" {
			continue
		}
		width := 0
		switch {
		case d == "google.com":
			width = 3
		case strings.HasSuffix(d, ".google.com"):
			width = 2
		case d == "gemini.google.com":
			width = 1
		default:
			width = 0
		}
		items = append(items, ckItem{name: ck.Name, val: ck.Value, domain: d, width: width})
	}
	best := map[string]ckItem{}
	for _, it := range items {
		cur, ok := best[it.name]
		if !ok || it.width > cur.width {
			best[it.name] = it
		}
	}
	var parts []string
	for _, it := range best {
		parts = append(parts, it.name+"="+it.val)
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("CDP 未返回任何 google cookie")
	}
	// 按模板固定顺序排关键项，其余按名字排序，保证串稳定
	return orderCookieString(parts), nil
}

// cdpEval 在页面里执行 JS，返回字符串结果（用于判登录态 / SNlM0e）。
func cdpEval(hostPort, expr string) (string, error) {
	ws, err := cdpListPages(hostPort)
	if err != nil {
		return "", err
	}
	pg, err := cdpDial(ws, 15*time.Second)
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

// orderCookieString 把关键项按固定顺序放前面，其余按名字排序。
func orderCookieString(parts []string) string {
	keyOrder := []string{
		"SID", "HSID", "SSID", "APISID", "SAPISID", "__Secure-1PSID",
		"__Secure-1PSIDTS", "__Secure-1PAPISID", "__Secure-1PSIDCC",
	}
	idx := map[string]int{}
	for i, k := range keyOrder {
		idx[k] = i
	}
	keyed := map[string]string{}
	var rest []string
	for _, p := range parts {
		name := p
		if i := strings.Index(p, "="); i > 0 {
			name = p[:i]
		}
		if _, ok := idx[name]; ok {
			keyed[name] = p
		} else {
			rest = append(rest, p)
		}
	}
	sortStrings(rest)
	var out []string
	for _, k := range keyOrder {
		if v, ok := keyed[k]; ok {
			out = append(out, v)
		}
	}
	out = append(out, rest...)
	return strings.Join(out, "; ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ── 高层：抓取并入库 ────────────────────────────────────────────────────────

// browserRefreshOne 对一个 browser 来源的账号做一次抓取：
//  1. 控制器确保 profile 实例在跑
//  2. CDP 导航 gemini.google.com
//  3. 判登录态（SNlM0e）
//  4. 抓 cookie 拼串 → 更新池记录（或新增）
//
// 返回 (是否成功, 描述)。
func browserRefreshOne(label, profile string) (bool, string, error) {
	if !browserEnabled() {
		return false, "", fmt.Errorf("浏览器登录未启用")
	}
	port, err := ensureBrowserProfile(profile)
	if err != nil {
		return false, "", err
	}
	hostPort := fmt.Sprintf("%s:%d", browserCDPHost(), port)

	// 导航到 gemini.google.com 并等加载
	if err := cdpNavigate(hostPort, "https://gemini.google.com/"); err != nil {
		return false, "", err
	}
	// 判登录态。必须把「网络不通/页面没加载出来」和「确实没登录」分开：
	// 断网时页面是 chrome-error://chromewebdata/，HTML 里当然没有 SNlM0e，
	// 若据此报「未登录」，上层 browserAutoRefresh 会把账号删掉，
	// 表现为「浏览器登录缓存每次被清除」。所以先认错误页，再谈登录态。
	loggedIn := false
	sawErrorPage := false
	evalFails := 0
	for i := 0; i < 12; i++ {
		has, err := cdpEval(hostPort, `(function(){
			var u = String(location.href || '');
			var h = document.documentElement ? document.documentElement.outerHTML : '';
			return JSON.stringify({
				err: u.indexOf('chrome-error://') === 0 || h.indexOf('ERR_CONNECTION') !== -1 || h.indexOf('ERR_PROXY') !== -1 || h.indexOf('ERR_NAME_NOT_RESOLVED') !== -1 || h.indexOf('ERR_TUNNEL_CONNECTION_FAILED') !== -1 || h.indexOf('ERR_TIMED_OUT') !== -1 || h.indexOf('This site can') !== -1,
				token: h.indexOf('SNlM0e') !== -1
			});
		})()`)
		if err != nil {
			evalFails++
		} else {
			s := strings.TrimSpace(has)
			s = strings.Trim(s, `"`)
			s = strings.ReplaceAll(s, `\"`, `"`)
			if strings.Contains(s, `"err":true`) {
				sawErrorPage = true
				break
			}
			if strings.Contains(s, `"token":true`) {
				loggedIn = true
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if sawErrorPage {
		return false, "", errBrowserPageUnreachable
	}
	if !loggedIn {
		// 页面能读但没有 SNlM0e：再看 cookie 是否仍带完整登录态。
		// SPA 有时还没渲染完，cookie 其实已经是好的。
		if ck, cerr := cdpGetCookies(hostPort); cerr == nil && extractSAPISID(ck) != "" && strings.Contains(ck, "__Secure-1PSID=") {
			loggedIn = true
		}
	}
	if !loggedIn {
		if evals := evalFails; evals >= 12 {
			// 一次都没读到页面：CDP/页面本身有问题，别赖登录态
			return false, "", errBrowserPageUnreachable
		}
		return false, "未登录", errBrowserNotLoggedIn
	}

	cookie, err := cdpGetCookies(hostPort)
	if err != nil {
		return false, "", err
	}
	if extractSAPISID(cookie) == "" {
		return false, "抓到 cookie 但缺 SAPISID", fmt.Errorf("抓到的 cookie 缺 SAPISID")
	}

	// 入库：找到同 profile 的 browser 来源行就更新，否则新建
	if err := browserStoreCookie(label, profile, cookie); err != nil {
		return false, "", err
	}
	return true, "已抓取并入库", nil
}

// browserCDPHost 返回 CDP 要连的主机：控制器和 chromium 同网络时用容器名。
// 若 BROWSER_CDP_HOST 配置了（比如控制器与 CDP 不在同机），用配置值。
func browserCDPHost() string {
	if strings.TrimSpace(cfg.BrowserCDPHost) != "" {
		return strings.TrimSpace(cfg.BrowserCDPHost)
	}
	// 控制器 URL 的主机部分
	u := browserControllerURL()
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexAny(rest, ":/"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return "chromium"
}

// browserStoreCookie 把抓到的 cookie 写进 accounts 表（按 profile 幂等）。
func browserStoreCookie(label, profile, cookie string) error {
	// 找已存在的 browser 来源 + 同 profile 的行
	var id int64
	err := getDB().QueryRow(
		`SELECT id FROM accounts WHERE source='browser' AND profile=? LIMIT 1`, profile).Scan(&id)
	if err == nil && id > 0 {
		// 更新 cookie，并清错误/失败
		_, e := getDB().Exec(
			`UPDATE accounts SET cookie=?, label=?, last_ok_at=?, last_error='', fail_count=0,
			     last_used_at=last_used_at
			 WHERE id=?`,
			cookie, strings.TrimSpace(label), time.Now().Unix(), id)
		if e != nil {
			return e
		}
		logf("[browser] profile %q cookie 已刷新 -> 账号 #%d", profile, id)
		return nil
	}
	// 不存在：新建。note 标来源。
	note := "浏览器登录自动导入"
	if label == "" {
		label = profile
	}
	nid, e := insertID(
		`INSERT INTO accounts(label, cookie, status, note, source, profile, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		strings.TrimSpace(label), cookie, "enabled", note, "browser", profile, time.Now().Unix())
	if e != nil {
		return e
	}
	logf("[browser] profile %q cookie 已入库 -> 新账号 #%d", profile, nid)
	return nil
}

// browserDeleteByProfile 删除某 profile 对应的池记录（登录态没了/手动删账号）。
func browserDeleteByProfile(profile string) {
	_, _ = getDB().Exec(`DELETE FROM accounts WHERE source='browser' AND profile=?`, profile)
}

// browserAccounts 返回所有 source='browser' 的账号（含 profile 名）。
func browserAccounts() []BrowserAccount {
	rows, err := getDB().Query(
		`SELECT id, label, cookie, status, note, created_at, last_used_at, last_ok_at,
		        last_error, fail_count, proxy_id, profile, source
		 FROM accounts WHERE source='browser' ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []BrowserAccount
	for rows.Next() {
		var a BrowserAccount
		var prof, src string
		if err := rows.Scan(&a.ID, &a.Label, &a.Cookie, &a.Status, &a.Note,
			&a.CreatedAt, &a.LastUsedAt, &a.LastOkAt, &a.LastError,
			&a.FailCount, &a.ProxyID, &prof, &src); err != nil {
			continue
		}
		a.Profile = prof
		a.Source = src
		out = append(out, a)
	}
	return out
}

// browserAutoRefresh 后台定时任务：对每个 browser 来源的账号做保活/刷新。
// 抓不到登录态就自动删除（用户要求：过期会话 cookie 自动删除）。
func browserAutoRefresh() {
	if !browserEnabled() {
		return
	}
	accts := browserAccounts()
	if len(accts) == 0 {
		return
	}
	ok, _, errStr := browserHealth()
	if !ok {
		logf("[browser] 控制器不可用，跳过自动刷新: %s", errStr)
		return
	}
	for _, a := range accts {
		if a.Status != "enabled" {
			continue
		}
		_, _, err := browserRefreshOne(a.Label, a.Profile)
		if err != nil {
			// 页面不可达（断网/代理挂/CDP 异常）：只记日志，**不累加 fail_count、不删号**。
			// 一断网就把好号清空，是「登录缓存每次被清除」的根源。
			if errors.Is(err, errBrowserPageUnreachable) {
				logf("[browser] 账号 #%d (profile %q) 页面不可达，保留账号等待恢复: %v", a.ID, a.Profile, err)
				continue
			}
			// 只有确认「未登录/缺 SAPISID」才动账号。
			if errors.Is(err, errBrowserNotLoggedIn) || strings.Contains(err.Error(), "缺 SAPISID") {
				logf("[browser] 账号 #%d (profile %q) 登录态已失效，自动删除", a.ID, a.Profile)
				_ = accountDelete(a.ID)
				continue
			}
			logf("[browser] 账号 #%d (profile %q) 自动刷新失败: %v", a.ID, a.Profile, err)
			continue
		}
		markAccountResult(a.ID, true, "")
	}
}

// startBrowserAutoRefresh 在 scheduler 里挂一个每 10 分钟跑一次的定时器。
func startBrowserAutoRefresh() {
	go func() {
		interval := time.Duration(browserRefreshMinutes()) * time.Minute
		if interval <= 0 {
			interval = 10 * time.Minute
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			browserAutoRefresh()
		}
	}()
}

// browserRefreshMinutes 返回自动刷新间隔（分钟）。
func browserRefreshMinutes() int {
	// 面板运行时配置优先
	m := rtCfg().BrowserRefreshMinutes
	if m <= 0 {
		m = cfg.BrowserRefreshMinutes
	}
	if m <= 0 {
		return 10
	}
	return m
}

// ctxNoCancel 供未来扩展。
func ctxNoCancel() context.Context { return context.Background() }
