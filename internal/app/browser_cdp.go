package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// errBrowserNeedRelogin：Google 在浏览器里弹出了重新验证（密码 challenge /
// 两步验证 / 异常活动确认）。这跟「未登录」是两码事：会话被服务端挂起，等用户
// 在 VNC 桌面里重新输一次密码就能恢复。2026-09-13 实测：账号被 Google 风控
// 强制重验后，页面停在 accounts.google.com/v3/signin/challenge/pwd，此时 CDP
// 抓到的 cookie 看着齐全（SAPISID / 1PSID 都在）但全是死的 —— 绝不能拿这种
// cookie 入库，也不能按「登录态失效」把账号删了。
var errBrowserNeedRelogin = errors.New("浏览器需要重新登录（Google 要求重新验证密码，请在 VNC 桌面完成）")

// errBrowserRefreshCooldown：同一个 profile 两次「导航刷新 + 抓取」之间隔得太近。
// 上层（自动刷新 / 检测自愈 / 手动抓取按钮）都会走到 browserRefreshOne，不加闸门
// 的话一次会话过期就能在几分钟内触发好几次页面刷新 —— 节点加载本来就慢，多处
// 刷新叠在一起正是触发 Google 风控的节奏。冷却内的调用一律拒绝，不做导航。
var errBrowserRefreshCooldown = errors.New("浏览器刷新冷却中")

// ── 抓取节奏（反风控核心参数）────────────────────────────────────────────────
const (
	// browserRefreshCooldownSec 是同一 profile 两次抓取尝试的最小间隔（含失败的
	// 尝试：失败往往说明页面/风控正处在敏感状态，更不该接着刷）。2026-09-13
	// 由 60s 提到 120s，与扩展 / refresh.py 的冷却窗口同步。
	browserRefreshCooldownSec = 120

	// browserRefreshSafetySec 抓取计划 = cookie 有效期 - 该安全余量。
	browserRefreshSafetySec = 300

	// browserRefreshMaxGapSec 是两次抓取的最大间隔。CDP 读到的 jar 有效期普遍
	// 是一年上下（那不是 1PSIDTS 票据的真实寿命），不设上限的话「有效期-5分钟」
	// 会排到一年以后。票据的日常续新由 POST /RotateCookies（rotate.go）在进程内
	// 完成，不经过浏览器，所以浏览器抓取最长一小时一次足够兜底。
	browserRefreshMaxGapSec = 3600
)

// kvKeyBrowserNextRefresh 按 profile 记下次该抓取的时刻（unix 秒）。
// 放 kv 而不是内存：重启后调度不丢节奏。
func kvKeyBrowserNextRefresh(profile string) string {
	return "browser_next_refresh:" + profile
}

// browserNextRefreshAt 取某 profile 计划的下一次抓取时刻，没记录返回 0。
func browserNextRefreshAt(profile string) int64 {
	v := kvGet(kvKeyBrowserNextRefresh(profile))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// browserScheduleNextRefresh 排下一次抓取。
//
// 节奏（2026-09-16 用户要求「间隔可在设置里自定义，单位分钟」）：
//   - 设置里 browser_refresh_minutes > 0 时按**固定间隔**排（用户显式配的节奏优先）；
//   - 否则按「关键 cookie 有效期 - 5 分钟」动态排，读不到有效期退回默认间隔。
//
// 结果统一钳制在 [冷却间隔, 最大间隔]：再急不得小于冷却（反风控），
// 再富余不超过一小时（jar 有效期不等于票据寿命，不能真按一年排）。
func browserScheduleNextRefresh(profile string, minExpiryUnix int64) {
	now := time.Now().Unix()
	var candidate int64
	if configured := browserRefreshMinutesConfigured(); configured > 0 {
		candidate = now + int64(configured)*60
	} else {
		candidate = now + int64(browserRefreshMinutes())*60
		if minExpiryUnix > 0 {
			candidate = minExpiryUnix - browserRefreshSafetySec
		}
	}
	if lo := now + browserRefreshCooldownSec; candidate < lo {
		candidate = lo
	}
	if hi := now + browserRefreshMaxGapSec; candidate > hi {
		candidate = hi
	}
	_ = kvSet(kvKeyBrowserNextRefresh(profile), strconv.FormatInt(candidate, 10))
}

// browserRefreshGate 抓取入口的冷却闸门。返回 (是否放行, 剩余秒数)。
// 同一 profile 并发调用也安全（锁内读改写）。
func browserRefreshGate(profile string) (bool, int64) {
	browserRefreshMu.Lock()
	defer browserRefreshMu.Unlock()
	now := time.Now().Unix()
	if last, ok := browserLastRefreshAt[profile]; ok {
		if remain := int64(browserRefreshCooldownSec) - (now - last); remain > 0 {
			return false, remain
		}
	}
	browserLastRefreshAt[profile] = now
	return true, 0
}

var (
	browserRefreshMu     sync.Mutex
	browserLastRefreshAt = map[string]int64{} // profile -> 上次抓取尝试（含失败）的时刻
)

func browserEnabled() bool {
	return strings.TrimSpace(cfg.BrowserControllerURL) != ""
}

// browserControllerURL 返回控制器基址（去掉尾部 /）。
func browserControllerURL() string {
	return strings.TrimRight(strings.TrimSpace(cfg.BrowserControllerURL), "/")
}

// ── 轻量 HTTP 帮助 ──────────────────────────────────────────────────────────

func browserHTTP(method, url string, body []byte, out interface{}) (int, error) {
	// 45s 而不是 20s：POST /profiles 在控制器里要 chown -R 整个 profile 目录、
	// 再拉起 Chromium，profile 一大就是十几秒；20s 时实测反复撞
	// "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"，
	// 表面上是「控制器不可用」，其实控制器正在干活。
	client := &http.Client{Timeout: 45 * time.Second}
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
//
// ★ 端口扫描兜底（2026-09-17 用户要求）★
// 控制器可能在「返回的端口」与实际监听端口不一致的情况下运行（profile 被
// 重启后换了端口、控制器自己重启过、配置漂移等）。此时按返回端口连过去会
// 失败，上层只能报「CDP 端口上没有 page 目标」这类看不清原因的错误。
// 所以这里做两层：拿到端口后**扫一遍控制器里该 profile 的实际端口**，
// 以实际可连的那个为准。
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
	// 端口扫描：控制器 /profiles 列出的才是实际在跑的端口
	if real := browserScanProfilePort(name); real > 0 && real != out.Port {
		logf("[browser] profile %q 控制器返回端口 %d，实际监听 %d，采用实际端口", name, out.Port, real)
		return real, nil
	}
	if out.Port <= 0 {
		// 控制器没返回端口：扫一遍看有没有在跑的
		if real := browserScanProfilePort(name); real > 0 {
			logf("[browser] 控制器未返回端口，扫描到 profile %q 实际监听 %d", name, real)
			return real, nil
		}
		return 0, fmt.Errorf("控制器未返回 CDP 端口")
	}
	return out.Port, nil
}

// browserScanProfilePort 扫描控制器里某 profile 的实际 CDP 端口，取可连通的那个。
//
// 数据源是控制器 GET /profiles（它的 state 里记着实例真实端口），
// 再用 GET /cdp/<port>/json/version 验证端口确实可连 —— 只信「列出来且连得上」
// 的端口，避免拿到已死实例的端口号。
func browserScanProfilePort(name string) int {
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
		if p.Name != name || p.Port <= 0 {
			continue
		}
		// 验证端口可连（/json/version 是 CDP 的探活端点）
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(cdpTunnelBase(p.Port) + "/json/version")
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return p.Port
		}
	}
	return 0
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

// cdpListPages 取 CDP 端口上的 page 目标，返回最合适的（隧道）ws url。
// 优先选已停在 gemini.google.com 的 tab：多 tab 时第一个目标可能是 about:blank
// 或设置页，在错误的 tab 上 evaluate 登录态判断恒为「未登录」。
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
	body, _ := io.ReadAll(resp.Body)
	// 控制器隧道在 profile 重启换端口后回 404 JSON 对象（{"error":...}），不是数组。
	// 直接 Unmarshal 到切片会报 "cannot unmarshal object into Go value of type []...",
	// 完全看不出是端口过期，这里先把这种情形翻成人话。
	var list []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
		WS   string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("CDP /json/list 响应异常（HTTP %d）: %s",
			resp.StatusCode, truncate(strings.TrimSpace(string(body)), 160))
	}
	var fallback string
	for _, t := range list {
		if t.Type != "page" || t.WS == "" {
			continue
		}
		ws := cdpRewriteWS(t.WS, port)
		if strings.Contains(t.URL, "gemini.google.com") {
			return ws, nil
		}
		if fallback == "" {
			fallback = ws
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("CDP 端口上没有 page 目标")
}

// cleanupBlankTabs 关掉多余的 about:blank 空白页（保留至多一个）。
//
// ★ 2026-09-17 用户反馈「一直新建页面」★
// 历史版本的 cdpNavigate 在 /json/list 瞬时失败时会 cdpOpenTab，这些页面
// 若没被导航走就留在 about:blank —— 桌面上、CDP 目标列表里越堆越多
// （实测 acct1 里就挂着一个残留 about:blank）。每次抓取前顺手清理，
// 保持「一个 profile 一个 Gemini 页 + 至多一个空白页」的干净状态。
func cleanupBlankTabs(hostPort string) int {
	port, err := cdpPortOfHostPort(hostPort)
	if err != nil {
		return 0
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(cdpTunnelBase(port) + "/json/list")
	if err != nil {
		return 0
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var list []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if json.Unmarshal(body, &list) != nil {
		return 0
	}
	closed := 0
	seenBlank := 0
	for _, t := range list {
		if t.Type != "page" || !strings.HasPrefix(t.URL, "about:") {
			continue
		}
		seenBlank++
		// 留第一个空白页（Chromium 总需要一个可用的初始 page），其余关掉
		if seenBlank == 1 || t.ID == "" {
			continue
		}
		cr, err := client.Get(cdpTunnelBase(port) + "/json/close/" + t.ID)
		if err == nil {
			cr.Body.Close()
			if cr.StatusCode == 200 {
				closed++
			}
		}
	}
	if closed > 0 {
		logf("[browser] 清理了 %d 个残留空白页", closed)
	}
	return closed
}

// cdpOpenTab 开一个新 tab 并导航，返回该 page 的（隧道）ws url。
// 用 HTTP /json/new?url 接口（PUT）。
//
// Chromium 正在退出/重启时 /json/new 可能返回 200 + 一个错误 JSON（或整个非
// JSON 体），旧代码只报「新建 tab 未返回 ws url」，完全看不出原因；而且没有任何
// 重试 —— 但恰好此时另一个调用已经在拉起实例，1 秒后再试基本就好。所以这里
// 读出响应体、失败带原因，并重试一次，仍失败再退回 /json/list 碰运气。
func cdpOpenTab(hostPort, url string) (string, error) {
	port, err := cdpPortOfHostPort(hostPort)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var lastBody string
	for retry := 0; retry < 2; retry++ {
		if retry > 0 {
			time.Sleep(time.Second)
		}
		newURL := cdpTunnelBase(port) + "/json/new?" + urlQueryEscape(url)
		req, _ := http.NewRequest(http.MethodPut, newURL, nil)
		resp, err := client.Do(req)
		if err != nil {
			lastBody = err.Error()
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var out struct {
			WS string `json:"webSocketDebuggerUrl"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			lastBody = truncate(strings.TrimSpace(string(body)), 160)
			continue
		}
		if out.WS == "" {
			lastBody = truncate(strings.TrimSpace(string(body)), 160)
			continue
		}
		return cdpRewriteWS(out.WS, port), nil
	}
	// 兜底：也许 tab 其实开出来了，只是 /json/new 的响应体没按规矩来
	if ws, err := cdpListPages(hostPort); err == nil {
		return ws, nil
	}
	return "", fmt.Errorf("新建 tab 失败: %s", lastBody)
}

func urlQueryEscape(s string) string {
	// 简单编码：只处理空格与特殊字符，够用即可
	replacer := strings.NewReplacer(" ", "%20", "\"", "%22", "'", "%27", "<", "%3C", ">", "%3E")
	return replacer.Replace(s)
}

// cdpNavigate 导航到 url（优先复用现有 page，仅确认没有任何 page 时才开新 tab）。
//
// ★ 不要轻易开新 tab（2026-09-17 用户反馈）★
// 服务器 Chromium 桌面上开始堆积多个 Gemini 窗口：/json/list 瞬时失败（WS 忙、
// profile 刚拉起还没监听）时旧逻辑立刻 cdpOpenTab —— 每次"抓取"都多一个窗口。
// 现在先重试 list 两次；确认「确实没有任何 page」才开新 tab。已有 page 时
// Page.navigate 就是"刷新该页"，不会产生新窗口。
func cdpNavigate(hostPort, url string) error {
	ws, err := cdpListPages(hostPort)
	if err != nil {
		// list 失败多为瞬时状态，重试而不是立刻开新 tab
		for retry := 0; retry < 2 && err != nil; retry++ {
			time.Sleep(2 * time.Second)
			ws, err = cdpListPages(hostPort)
		}
		if err != nil {
			// 确认没有任何可复用的 page，才开新 tab
			ws, err = cdpOpenTab(hostPort, url)
			if err != nil {
				return err
			}
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

// cdpCookieCapture 是一次 CDP 抓取的产出：cookie 串 + 关键轮换 cookie 的最早有效期。
type cdpCookieCapture struct {
	Cookie string
	// MinExpiryUnix 是 *PSIDTS / SIDCC / *PSIDCC 这些「负责维持会话新鲜」的
	// cookie 里最早到期的时刻（unix 秒）；0 = 没读到有效期。它是「按有效期-5
	// 分钟排下一次抓取」的依据。注意 jar 有效期普遍长达一年，别当成 1PSIDTS
	// 票据的服务端寿命 —— 票据续新靠 POST /RotateCookies，不靠这里。
	MinExpiryUnix int64
}

// cdpGetCookies 通过 CDP 抓取 host 相关域的 cookie，拼成 "k=v; k=v" header 串。
// 只保留登录态需要的域：gemini.google.com 及 .google.com 的会话 cookie。
func cdpGetCookies(hostPort string) (string, error) {
	cap, err := cdpCaptureCookies(hostPort)
	if err != nil {
		return "", err
	}
	return cap.Cookie, nil
}

func cdpCaptureCookies(hostPort string) (cdpCookieCapture, error) {
	ws, err := cdpListPages(hostPort)
	if err != nil {
		return cdpCookieCapture{}, err
	}
	pg, err := cdpDial(ws, 15*time.Second)
	if err != nil {
		return cdpCookieCapture{}, err
	}
	defer pg.Close()

	// Network.enable 让 cookie 域信息返回
	_, _ = pg.call("Network.enable", map[string]interface{}{})
	res, err := pg.call("Network.getAllCookies", map[string]interface{}{})
	if err != nil {
		return cdpCookieCapture{}, err
	}
	var data struct {
		Cookies []struct {
			Name    string  `json:"name"`
			Value   string  `json:"value"`
			Domain  string  `json:"domain"`
			Path    string  `json:"path"`
			Expires float64 `json:"expires"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(res, &data); err != nil {
		return cdpCookieCapture{}, err
	}
	// 同一名字可能出现在多个域（例如 SAPISID 同时挂在 .google.com 与
	// gemini.google.com）。浏览器发给 gemini.google.com 的 Cookie 头每个名字
	// 只带一个值 —— 域越宽优先级越高（.google.com 上的值才是被 Gemini 用于
	// 授权的那个）。所以收集时按「域宽度」保留优先级最高的那份。
	type ckItem struct {
		name, val, domain string
		width             int
		expiry            float64
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
		items = append(items, ckItem{name: ck.Name, val: ck.Value, domain: d, width: width, expiry: ck.Expires})
	}
	best := map[string]ckItem{}
	for _, it := range items {
		cur, ok := best[it.name]
		if !ok || it.width > cur.width {
			best[it.name] = it
		}
	}
	var parts []string
	minExpiry := float64(0)
	nowSec := float64(time.Now().Unix())
	for name, it := range best {
		parts = append(parts, name+"="+it.val)
		// 只关心「会话保鲜组」的到期时刻：这些是每次访问都会被服务端轮换的
		// 短周期 cookie，它们最早的那个到期时间决定了什么时候该再刷一次页面。
		switch name {
		case "__Secure-1PSIDTS", "__Secure-3PSIDTS", "SIDCC",
			"__Secure-1PSIDCC", "__Secure-3PSIDCC":
			if it.expiry > nowSec+60 && (minExpiry == 0 || it.expiry < minExpiry) {
				minExpiry = it.expiry
			}
		}
	}
	if len(parts) == 0 {
		return cdpCookieCapture{}, fmt.Errorf("CDP 未返回任何 google cookie")
	}
	// 按模板固定顺序排关键项，其余按名字排序，保证串稳定
	return cdpCookieCapture{Cookie: orderCookieString(parts), MinExpiryUnix: int64(minExpiry)}, nil
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

// pageStateJS 在页面里取登录态判断需要的全部信息：
//
//	err    —— chrome 错误页（断网 / 代理挂 / DNS 失败）
//	signin —— Google 把页面顶到了重新验证 / 登录流程（challenge 页）。此时就算
//	          cookie 看着齐全也是死的，绝不能抓回去入库。
//	token  —— 页面里有**非空**的 SNlM0e。注意必须是「值非空」：新版前端在匿名 /
//	          challenge 页也会带上 `"SNlM0e":""` 这个**空**键，老代码只查
//	          indexOf('SNlM0e') 会被它骗过去，反过来偶尔又因为 DOM 尚未水合漏报。
const pageStateJS = `(function(){
	var u = String(location.href || '');
	var h = document.documentElement ? document.documentElement.outerHTML : '';
	var w = '';
	try { w = String((typeof WIZ_global_data !== 'undefined' && WIZ_global_data && WIZ_global_data.SNlM0e) || ''); } catch (e) {}
	return JSON.stringify({
		err: u.indexOf('chrome-error://') === 0 || h.indexOf('ERR_CONNECTION') !== -1 || h.indexOf('ERR_PROXY') !== -1 || h.indexOf('ERR_NAME_NOT_RESOLVED') !== -1 || h.indexOf('ERR_TUNNEL_CONNECTION_FAILED') !== -1 || h.indexOf('ERR_TIMED_OUT') !== -1 || h.indexOf('This site can') !== -1,
		signin: u.indexOf('accounts.google.com') !== -1 && (u.indexOf('/signin') !== -1 || u.indexOf('ServiceLogin') !== -1 || u.indexOf('/challenge/') !== -1 || u.indexOf('/v3/signin') !== -1),
		token: (/"SNlM0e":"[^"]{10,}"/).test(h) || w.length >= 10
	});
})()`

// verifyByInPageFetch 兜底验证：直接在页面里 fetch('/app')（带浏览器自己的
// cookie 和出口），看响应 HTML 里有没有非空 SNlM0e。
//
// 为什么要这一步：SPA 渲染慢时 DOM 里可能还没有 token，而旧的兜底是「cookie 里
// 有 SAPISID + __Secure-1PSID 就当已登录」——这正是僵尸 cookie 的来源：Google
// 强制重验 / 登出的页面上那两项照样在，照单全收就把死 cookie 写进了池子，还覆盖掉
// 上一份好 cookie。改成页面内 fetch 验证后，只有**服务端真的认这份会话**才入库。
func verifyByInPageFetch(hostPort string) bool {
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		out, err := cdpEval(hostPort, `(async function(){
			try {
				var r = await fetch('/app', {credentials:'include', redirect:'follow'});
				var t = await r.text();
				return JSON.stringify({status: r.status, token: (/"SNlM0e":"[^"]{10,}"/).test(t)});
			} catch (e) {
				return JSON.stringify({status: 0, token: false, error: String(e)});
			}
		})()`)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(out)
		s = strings.Trim(s, `"`)
		s = strings.ReplaceAll(s, `\"`, `"`)
		if strings.Contains(s, `"token":true`) {
			return true
		}
	}
	return false
}

// browserRefreshOne 对一个 browser 来源的账号做一次抓取：
//
//  1. 冷却闸门：距上次抓取尝试（含失败）不足 browserRefreshCooldownSec 直接拒绝，
//     不导航 —— 自动刷新、检测自愈、手动按钮都可能同时想到达这里，多处刷新叠加
//     正是触发 Google 风控的节奏
//  2. 控制器确保 profile 实例在跑
//  3. **先刷新**：CDP 导航 gemini.google.com（这一步就是「刷新」），再判登录态
//  4. 只有登录态**验证通过**才抓 cookie 拼串入库（新增或更新）
//  5. 抓完不再做任何刷新动作，按「关键 cookie 有效期 - 5 分钟」把下一次抓取
//     写进 kv 排程，到点前 browserAutoRefresh 一律跳过
//
// 返回 (是否成功, 描述, 错误)。
//
// ★ 抓取模式（2026-09-16 用户要求）★
//
// 先「只读抓取」：不导航、不刷新页面，直接从现有页面/CDP 读 cookie。只有读到的
// cookie 验证失败（页面判匿名 / 缺关键项）才导航刷新一次再抓。这样日常抓取不再
// 反复刷新页面，显著降低风控暴露；刷新只发生在确实需要时。
//
// 顺序：冷却闸门 → 控制器确保实例在跑 → 只读抓取（读页面登录态 + 抓 cookie）
// → 失败才导航刷新 → 重读 → 入库 → 按有效期排程下次。
func browserRefreshOne(label, profile string) (bool, string, error) {
	if !browserEnabled() {
		return false, "", fmt.Errorf("浏览器登录未启用")
	}
	if ok, remain := browserRefreshGate(profile); !ok {
		return false, "", fmt.Errorf("%w：profile %q 距上次抓取尝试不足 %d 秒，剩余约 %d 秒",
			errBrowserRefreshCooldown, profile, browserRefreshCooldownSec, remain)
	}
	return browserRefreshCore(label, profile)
}

// browserRefreshCore 是抓取主体（不含闸门），供 browserRefreshOne（带闸门）
// 与 browserRefreshOneForce（校验失败后的强制重抓）共用。
func browserRefreshCore(label, profile string) (bool, string, error) {
	port, err := ensureBrowserProfile(profile)
	if err != nil {
		return false, "", err
	}
	hostPort := fmt.Sprintf("%s:%d", browserCDPHost(), port)

	// 顺手清理残留空白页：历史版本开过的 about:blank 会一直挂在 CDP 目标
	// 列表里（反复新建页面问题的遗留），每次抓取前收一遍。
	cleanupBlankTabs(hostPort)

	// ── 第一步：只读抓取（不导航、不刷新）────────────────────────────────
	// 直接从 Chromium 现有页面读登录态并抓 cookie。页面若停在别的 tab 或
	// 尚未加载完，这步会判「未登录」，随即进入第二步的刷新路径。
	if ok, detail := browserTryReadOnly(hostPort, label, profile); ok {
		return true, detail, nil
	} else {
		logf("[browser] profile %q 只读抓取未成功（%s），刷新页面后重试", profile, detail)
	}

	// ── 第二步：导航刷新后重抓 ──────────────────────────────────────────
	// 导航到 gemini.google.com 并等加载
	if err := cdpNavigate(hostPort, "https://gemini.google.com/"); err != nil {
		return false, "", err
	}
	// 必须把三种状态分开，它们的处置完全不同：
	//   错误页（断网/代理挂）        -> 保留现状只记日志
	//   Google 重新验证页           -> 保留账号、提示用户去 VNC 重新登录，绝不入库
	//   匿名 / SPA 未渲染完          -> 页面内 fetch 兜底，仍不行才算未登录
	loggedIn := false
	sawErrorPage := false
	sawSignin := false
	evalFails := 0
	for i := 0; i < 12; i++ {
		has, err := cdpEval(hostPort, pageStateJS)
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
			if strings.Contains(s, `"signin":true`) {
				sawSignin = true
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
	if sawSignin {
		return false, "", errBrowserNeedRelogin
	}
	if !loggedIn {
		// 页面能读但没有非空 SNlM0e：SPA 有时还没渲染完，用页面内 fetch('/app')
		// 再验证一次。验证不通过就老实报未登录 —— 宁可漏抓一次，也不把 Google
		// 重验页上的死 cookie 当成登录态写进池子（2026-09-13 实测踩过）。
		if verifyByInPageFetch(hostPort) {
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

	cap, err := cdpCaptureCookies(hostPort)
	if err != nil {
		return false, "", err
	}
	cookie := cap.Cookie
	if extractSAPISID(cookie) == "" {
		return false, "抓到 cookie 但缺 SAPISID", fmt.Errorf("抓到的 cookie 缺 SAPISID")
	}
	if !strings.Contains(cookie, "__Secure-1PSID=") {
		return false, "抓到 cookie 但缺 __Secure-1PSID", fmt.Errorf("抓到的 cookie 缺 __Secure-1PSID")
	}

	// 入库：找到同 profile 的 browser 来源行就更新，否则新建
	if err := browserStoreCookie(label, profile, cookie); err != nil {
		return false, "", err
	}
	// 抓完即收工：把下一次抓取按「有效期 - 5 分钟」排进 kv，
	// 在那之前不再碰浏览器（冷却 + 排程双闸门，见文件头节奏参数）。
	browserScheduleNextRefresh(profile, cap.MinExpiryUnix)
	return true, "已抓取并入库", nil
}

// browserTryReadOnly 只读抓取：不导航、不刷新，直接读现有页面的登录态并抓 cookie。
//
// 判据与刷新路径一致（非空 SNlM0e / 页面内 fetch 兜底），但**不做任何导航**。
//
// ★ 2026-09-17 调整抓取顺序：先 cookie、后页面 ★
// Network.getAllCookies 读的是**整个 profile 的 cookie jar**，不依赖当前页面
// 在哪个 tab —— 页面停在别的站点时照样能抓到完整登录态。旧顺序先查页面 URL
// / SNlM0e，页面不在 gemini 域或 SPA 未渲染完就判「只读失败」，然后走导航
// 路径，平白多刷新一次页面。改成先抓 cookie：有 SAPISID + 1PSID 就直接入库，
// 页面检查只作为「cookie 缺项时的二次确认」，绝大多数抓取完全零导航。
func browserTryReadOnly(hostPort, label, profile string) (bool, string) {
	// 第一步：直接抓 cookie jar（与页面所在 tab 无关）
	cap, err := cdpCaptureCookies(hostPort)
	if err == nil {
		cookie := cap.Cookie
		if extractSAPISID(cookie) != "" && strings.Contains(cookie, "__Secure-1PSID=") {
			if err := browserStoreCookie(label, profile, cookie); err == nil {
				browserScheduleNextRefresh(profile, cap.MinExpiryUnix)
				logf("[browser] profile %q 只读抓取成功（未刷新页面）", profile)
				return true, "已抓取并入库（未刷新页面）"
			}
		}
	}

	// 第二步：cookie 缺项/抓取失败 —— 用页面登录态做二次确认（仍然零导航）。
	cur, err := cdpEval(hostPort, `String(location.href || '')`)
	if err != nil {
		return false, "读取当前页面地址失败"
	}
	cur = strings.TrimSpace(strings.Trim(strings.TrimSpace(cur), `"`))
	if !strings.Contains(cur, "gemini.google.com") {
		return false, "当前页面不在 gemini.google.com（" + truncate(cur, 60) + "）"
	}
	has, err := cdpEval(hostPort, pageStateJS)
	if err != nil {
		return false, "读取页面登录态失败"
	}
	s := strings.TrimSpace(has)
	s = strings.Trim(s, `"`)
	s = strings.ReplaceAll(s, `\"`, `"`)
	if strings.Contains(s, `"err":true`) {
		return false, "页面是错误页"
	}
	if strings.Contains(s, `"signin":true`) {
		// 重验页：只读路径不处理，交给刷新路径给出「需要重新登录」的明确结论。
		return false, "页面停在 Google 登录/重验流程"
	}
	if strings.Contains(s, `"token":true`) {
		// 页面有登录态但 cookie jar 缺项：再抓一次（可能上一轮 jar 读取太早）
		cap2, err := cdpCaptureCookies(hostPort)
		if err == nil {
			if err := browserStoreCookie(label, profile, cap2.Cookie); err == nil {
				browserScheduleNextRefresh(profile, cap2.MinExpiryUnix)
				return true, "已抓取并入库（未刷新页面，页面确认后重抓）"
			}
		}
	}
	return false, "cookie 缺关键项（SAPISID / __Secure-1PSID）"
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

// browserStoreCookie 把抓到的 cookie 写进 accounts 表（容器抓取路径，source=browser）。
func browserStoreCookie(label, profile, cookie string) error {
	return browserStoreCookieSource(label, profile, cookie, "browser")
}

// browserStoreCookieSource 把 cookie 写进 accounts 表。
//
// ★ 去重按 profile 全局（2026-09-17 线上实测修正）★
// 同一个 Gemini 登录可能被两条路径写入：服务器 Chromium 抓取（source=browser）
// 和本机扩展推送（source=remote）。这是**同一个账号的两条供给路**，不是两个
// 账号 —— 按 (source, profile) 去重会让池子里出现 browser:browser1 和
// remote:browser1 两行，同一个号被轮转两份、配额算两遍。改为按 profile 全局
// 去重：最新写入获胜（source 跟随最新来源），同 profile 其它行合并删除
// （与服务器侧 pool.py 的策略一致）。
func browserStoreCookieSource(label, profile, cookie, source string) error {
	if source == "" {
		source = "browser"
	}
	var id int64
	var prevSource string
	err := getDB().QueryRow(
		`SELECT id, source FROM accounts WHERE profile=? ORDER BY id DESC LIMIT 1`,
		profile).Scan(&id, &prevSource)
	if err == nil && id > 0 {
		// 更新 cookie，并清错误/失败；source 跟随最新写入的来源
		_, e := getDB().Exec(
			`UPDATE accounts SET cookie=?, label=?, note=CASE WHEN ?<>'' THEN note ELSE note END,
			     last_ok_at=?, last_error='', fail_count=0, source=?,
			     last_used_at=last_used_at
			 WHERE id=?`,
			cookie, strings.TrimSpace(label), "", time.Now().Unix(), source, id)
		if e != nil {
			return e
		}
		logf("[browser] profile %q cookie 已刷新 -> 账号 #%d（source=%s，原 %s）", profile, id, source, prevSource)
		// 合并同 profile 的其它历史行（比如两条路径各自建过一行）
		if _, e := getDB().Exec(
			`DELETE FROM accounts WHERE profile=? AND id<>?`, profile, id); e != nil {
			logf("[browser] 合并 profile %q 旧记录失败: %v", profile, e)
		}
		return nil
	}
	// 不存在：新建。note 标来源。
	note := "浏览器登录自动导入"
	if source == "remote" {
		note = "远程浏览器扩展导入"
	}
	if label == "" {
		label = profile
	}
	nid, e := insertID(
		`INSERT INTO accounts(label, cookie, status, note, created_at, last_used_at, last_ok_at, last_error, fail_count, proxy_id, source, profile)
		 VALUES (?,?,'enabled',?,?,?,'',0,0,0,?,?)`,
		strings.TrimSpace(label), cookie, note, time.Now().Unix(), time.Now().Unix(), source, profile)
	if e != nil {
		return e
	}
	logf("[browser] profile %q cookie 已入库 -> 新账号 #%d（source=%s）", profile, nid, source)
	return nil
}

// browserDeleteByProfile 删除某 profile 对应的池记录（登录态没了/手动删账号）。
func browserDeleteByProfile(profile string) {
	_, _ = getDB().Exec(`DELETE FROM accounts WHERE source IN ('browser','remote') AND profile=?`, profile)
}

// browserAccounts 返回所有浏览器来源（容器抓取 browser / 远程扩展 remote）的账号。
func browserAccounts() []BrowserAccount {
	rows, err := getDB().Query(
		`SELECT id, label, cookie, status, note, created_at, last_used_at, last_ok_at,
		        last_error, fail_count, proxy_id, profile, source
		 FROM accounts WHERE source IN ('browser','remote') ORDER BY id`)
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
//
// 节奏由两层闸门控制（反风控）：
//   - kv 里按 profile 排程的 next_refresh（抓取成功时按「有效期 - 5 分钟」写入），
//     没到点直接跳过，不碰浏览器；
//   - browserRefreshOne 入口的冷却闸门，兜住「排程之外被别的路径拉起来」的情形。
//
// 登录态真没了（且不是 Google 重验挂起）才自动删除。
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
	now := time.Now().Unix()
	// 失败后把排程往后推：抓取失败往往说明页面/风控正处在敏感状态，
	// 每分钟重试一遍只会火上浇油。重验挂起（needRelogin）更要等用户先去
	// VNC 里重新登录，推 10 分钟。
	deferNext := func(profile string, sec int64) {
		_ = kvSet(kvKeyBrowserNextRefresh(profile),
			strconv.FormatInt(time.Now().Unix()+sec, 10))
	}
	for _, a := range browserAccounts() {
		// ★ remote 来源的账号归扩展负责（扩展自己保活+重推），浏览器刷新
		// 循环绝不碰它们（2026-09-17 线上事故）：remote 账号没有对应
		// Chromium profile，browserRefreshOne 对它必然报「未登录」，紧接着
		// 就把刚推送入池的新 cookie 删掉 —— 扩展推一次被删一次。
		if a.Source == "remote" {
			continue
		}
		if a.Status != "enabled" {
			continue
		}
		if next := browserNextRefreshAt(a.Profile); next > now {
			// 还没到「有效期 - 5 分钟」的排程点，不打扰浏览器
			continue
		}
		_, _, err := browserRefreshOne(a.Label, a.Profile)
		if err != nil {
			// 页面不可达（断网/代理挂/CDP 异常）：只记日志，**不累加 fail_count、不删号**。
			// 一断网就把好号清空，是「登录缓存每次被清除」的根源。
			if errors.Is(err, errBrowserPageUnreachable) || errors.Is(err, errBrowserRefreshCooldown) {
				if !errors.Is(err, errBrowserRefreshCooldown) {
					logf("[browser] 账号 #%d (profile %q) 页面不可达，保留账号等待恢复: %v", a.ID, a.Profile, err)
				}
				deferNext(a.Profile, browserRefreshCooldownSec)
				continue
			}
			// Google 在浏览器里弹重新验证（密码 challenge / 异常确认）：这**不是**
			// 登录态自然过期，是风控挂起，用户在 VNC 桌面重新输一次密码就恢复。
			// 此时不抓（挑战页上的 cookie 是死的）、不删号，把原因写到面板上。
			// 2026-09-13 实测：删了的话用户重新登录后还得手动重建账号。
			if errors.Is(err, errBrowserNeedRelogin) {
				logf("[browser] 账号 #%d (profile %q) 需要重新登录（Google 要求重新验证），已保留，请在 VNC 桌面完成", a.ID, a.Profile)
				_, _ = getDB().Exec(`UPDATE accounts SET last_error=? WHERE id=?`,
					"浏览器需要重新登录（Google 要求重新验证密码），请在 VNC 桌面完成", a.ID)
				deferNext(a.Profile, 600)
				continue
			}
			// 只有确认「未登录/缺 SAPISID」才动账号。
			if errors.Is(err, errBrowserNotLoggedIn) || strings.Contains(err.Error(), "缺 SAPISID") || strings.Contains(err.Error(), "缺 __Secure-1PSID") {
				logf("[browser] 账号 #%d (profile %q) 登录态已失效，自动删除", a.ID, a.Profile)
				_ = accountDelete(a.ID)
				continue
			}
			logf("[browser] 账号 #%d (profile %q) 自动刷新失败: %v", a.ID, a.Profile, err)
			deferNext(a.Profile, browserRefreshCooldownSec)
			continue
		}
		markAccountResult(a.ID, true, "")
	}
}

// startBrowserAutoRefresh 挂一个每分钟跑一次的轻量循环。
//
// 真正的抓取节奏不在 ticker 周期里，而在 kv 的 next_refresh 排程
// （按「cookie 有效期 - 5 分钟」动态计算，见 browserScheduleNextRefresh）：
// ticker 只负责每分钟醒来看一眼到没到点。排程没到就跳过，什么都不做。
func startBrowserAutoRefresh() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			browserAutoRefresh()
		}
	}()
}

// browserRefreshMinutes 返回抓取的兜底间隔（分钟）。
//
// 只在「本轮抓到的 cookie 读不到有效期」时用于排下一次；读得到有效期时排程
// 一律按「有效期 - 5 分钟」（钳制到 [冷却, 1 小时]）。面板运行时配置优先。
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

// browserRefreshMinutesConfigured 返回**用户显式配置**的抓取间隔（分钟）；
// 没配过返回 0。与 browserRefreshMinutes 的区别：后者带默认值 10，
// 用于「读不到有效期时的兜底」；本函数用于判断「用户是否要求固定间隔」。
func browserRefreshMinutesConfigured() int {
	if m := rtCfg().BrowserRefreshMinutes; m > 0 {
		return m
	}
	if m := cfg.BrowserRefreshMinutes; m > 0 {
		return m
	}
	return 0
}

// ctxNoCancel 供未来扩展。
func ctxNoCancel() context.Context { return context.Background() }
