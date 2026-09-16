package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
)

// ── Admin API：浏览器登录（Chromium profile）───────────────────────────────
//
// 路由（都走 requireAuth，挂在 /admin/api/browser*）：
//
//	GET    /admin/api/browser/status         控制器健康 + profile 列表 + 关联账号
//	POST   /admin/api/browser/profiles       { name,label? } 建/确保 profile
//	POST   /admin/api/browser/profiles/<name>/open     用 CDP 打开 gemini 登录页（供 VNC 手输）
//	POST   /admin/api/browser/profiles/<name>/fetch    立即抓 cookie 入库（含模型校验）
//	DELETE /admin/api/browser/profiles/<name>          停实例 + 删对应池记录
//	GET    /admin/api/browser/env           部署环境识别（docker? 浏览器? 系统类型）
//	GET    /admin/api/browser/access-url    读服务端保存的桌面网页地址
//	PUT    /admin/api/browser/access-url    写服务端保存的桌面网页地址（存 kv，跨浏览器）
//	GET    /admin/api/browser/extension     下载抓取扩展 zip（远程浏览器场景）
//
// profile 名就是「账号隔离单元」：每个 profile 对应 chromium 容器里一个独立
// Chromium user-data-dir + CDP 端口，登录态互不相通。

// kvBrowserAccessURL 是「桌面网页地址」在服务端的存储键。
// 用户要求：保存的地址写入服务端数据，而不是保存在用户浏览器的 localStorage。
const kvBrowserAccessURL = "browser_access_url"

// handleAdminBrowserStatus — GET /admin/api/browser/status
func handleAdminBrowserStatus(w http.ResponseWriter, r *http.Request) {
	enabled := browserEnabled()
	healthy := false
	errStr := ""
	if enabled {
		var ok bool
		ok, _, errStr = browserHealth()
		healthy = ok
	} else {
		errStr = "未配置 BROWSER_CONTROLLER_URL，浏览器登录未启用"
	}

	// 控制器里实际在跑的 profile
	ctrlNames := map[string]bool{}
	ctrlList := []map[string]interface{}{}
	if enabled {
		var out struct {
			Profiles []struct {
				Name string `json:"name"`
				Port int    `json:"port"`
			} `json:"profiles"`
		}
		code, err := browserHTTP(http.MethodGet, browserControllerURL()+"/profiles", nil, &out)
		if err == nil && code == 200 {
			for _, p := range out.Profiles {
				ctrlNames[p.Name] = true
				ctrlList = append(ctrlList, map[string]interface{}{
					"name": p.Name, "port": p.Port,
				})
			}
		}
	}

	// 关联账号（source=browser）
	browserAccts := browserAccounts()
	acctByProfile := map[string]BrowserAccount{}
	for _, a := range browserAccts {
		acctByProfile[a.Profile] = a
	}

	// 并集：DB 里有记录的 profile + 控制器里在跑的 profile
	profileSet := map[string]bool{}
	for _, a := range browserAccts {
		if a.Profile != "" {
			profileSet[a.Profile] = true
		}
	}
	for n := range ctrlNames {
		profileSet[n] = true
	}
	var names []string
	for n := range profileSet {
		names = append(names, n)
	}
	sort.Strings(names)

	items := []map[string]interface{}{}
	for _, name := range names {
		a, hasAcct := acctByProfile[name]
		view := map[string]interface{}{
			"name":       name,
			"port":       0,
			"running":    ctrlNames[name],
			"account_id": 0,
			"label":      name,
			"status":     "",
			"has_cookie": false,
			"last_ok_at": int64(0),
			"last_error": "",
			"created_at": int64(0),
		}
		for _, c := range ctrlList {
			if c["name"] == name {
				view["port"] = c["port"]
			}
		}
		if hasAcct {
			view["account_id"] = a.ID
			view["label"] = a.Label
			view["status"] = a.Status
			view["has_cookie"] = strings.TrimSpace(a.Cookie) != ""
			view["last_ok_at"] = a.LastOkAt
			view["last_error"] = a.LastError
			view["created_at"] = a.CreatedAt
			if a.Label != "" {
				view["label"] = a.Label
			}
		}
		items = append(items, view)
	}

	writeJSON(w, 200, map[string]interface{}{
		"enabled":    enabled,
		"healthy":    healthy,
		"detail":     errStr,
		"profiles":   items,
		"ctrl":       ctrlList,
		"access_url": browserAccessURL(),
	})
}

// browserAccessURL 返回当前生效的桌面网页地址：服务端 kv 里用户保存过的优先，
// 否则回落到启动参数 BROWSER_ACCESS_URL。存服务端（而不是浏览器 localStorage）
// 让同一份配置对所有访问面板的人/设备生效。
func browserAccessURL() string {
	if v := strings.TrimSpace(kvGet(kvBrowserAccessURL)); v != "" {
		return v
	}
	return strings.TrimSpace(cfg.BrowserAccessURL)
}

// handleAdminBrowserAccessURL — GET/PUT /admin/api/browser/access-url
func handleAdminBrowserAccessURL(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]interface{}{
			"access_url": browserAccessURL(),
			"from_kv":    strings.TrimSpace(kvGet(kvBrowserAccessURL)) != "",
			"default":    strings.TrimSpace(cfg.BrowserAccessURL),
		})
	case http.MethodPut:
		var p struct {
			AccessURL string `json:"access_url"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		v := strings.TrimSpace(p.AccessURL)
		if err := kvSet(kvBrowserAccessURL, v); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		logf("[browser] 桌面网页地址已更新为 %q（存服务端）", v)
		writeJSON(w, 200, map[string]interface{}{"ok": true, "access_url": browserAccessURL()})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// handleAdminBrowserProfiles — POST /admin/api/browser/profiles
func handleAdminBrowserProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var p struct {
		Name  string `json:"name"`
		Label string `json:"label"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &p); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		writeJSON(w, 400, map[string]string{"error": "profile 名不能为空"})
		return
	}
	port, err := ensureBrowserProfile(p.Name)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	// 打开登录页方便 VNC 手输（best-effort）
	_ = browserOpenLogin(p.Name)
	writeJSON(w, 200, map[string]interface{}{
		"name": p.Name, "port": port, "ok": true,
		"detail": "profile 已就绪，请在 Chromium VNC 桌面（:3000/:3001）登录该账号",
	})
}

// handleAdminBrowserProfileAction — POST/DELETE /admin/api/browser/profiles/<name>/<action>
func handleAdminBrowserProfileAction(w http.ResponseWriter, r *http.Request) {
	prefix := "/admin/api/browser/profiles/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || parts[0] == "" {
		writeJSON(w, 404, map[string]string{"error": "missing profile name"})
		return
	}
	name := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch r.Method {
	case http.MethodDelete:
		if err := browserStopProfile(name); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		// 删池记录 + 停实例
		browserDeleteByProfile(name)
		logf("[browser] profile %q 已停止并清理", name)
		writeJSON(w, 200, map[string]bool{"ok": true})

	case http.MethodPost:
		switch action {
		case "open":
			// 先确保实例在跑，再导航到登录页
			if _, err := ensureBrowserProfile(name); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			if err := browserOpenLogin(name); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, 200, map[string]interface{}{
				"ok":     true,
				"detail": "已在对应 Chromium 窗口打开 gemini.google.com，请在 VNC 桌面登录",
			})
		case "fetch":
			// 立即抓 cookie 入库，随后用默认模型做一次真实请求校验
			// （用户要求：抓取后必须验证这条链路真能用）。
			acct, _ := findBrowserAccountByProfile(name)
			label := name
			if acct != nil && acct.Label != "" {
				label = acct.Label
			}
			ok, detail, err := browserRefreshOne(label, name)
			if err != nil {
				writeJSON(w, 200, map[string]interface{}{"ok": false, "detail": err.Error()})
				return
			}
			_ = detail
			nacct, _ := findBrowserAccountByProfile(name)
			id := int64(0)
			if nacct != nil {
				id = nacct.ID
			}
			// ── 抓取后模型校验 ────────────────────────────────────────────
			// 校验失败时按 302 / 其它失败两种路径自愈（重置代理池、重抓）
			vr := browserVerifyAfterFetch(label, name, true)
			resp := map[string]interface{}{
				"ok":         ok && vr.OK,
				"account_id": id,
				"detail":     fmt.Sprintf("cookie 已抓取并写入 cookie 池（账号 #%d）", id),
				"verify":     vr,
			}
			if !vr.OK {
				resp["detail"] = fmt.Sprintf("cookie 已入库（账号 #%d），但默认模型校验未通过：%s", id, vr.Detail)
			} else if detail != "" {
				resp["detail"] = fmt.Sprintf("cookie 已抓取并写入 cookie 池（账号 #%d）· %s", id, vr.Detail)
			}
			writeJSON(w, 200, resp)
		default:
			writeJSON(w, 400, map[string]string{"error": "unknown action"})
		}
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

// browserOpenLogin 导航到 gemini.google.com 登录页。
func browserOpenLogin(profile string) error {
	port, err := ensureBrowserProfile(profile)
	if err != nil {
		return err
	}
	hostPort := fmt.Sprintf("%s:%d", browserCDPHost(), port)
	return cdpNavigate(hostPort, "https://gemini.google.com/")
}

// findBrowserAccountByProfile 按 profile 找 browser 来源账号。
func findBrowserAccountByProfile(profile string) (*BrowserAccount, error) {
	accts := browserAccounts()
	for i := range accts {
		if accts[i].Profile == profile {
			return &accts[i], nil
		}
	}
	return nil, fmt.Errorf("no account for profile %q", profile)
}

// handleAdminBrowserNow — POST /admin/api/browser/refresh 手动对所有 browser 账号刷新
func handleAdminBrowserNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	accts := browserAccounts()
	if len(accts) == 0 {
		writeJSON(w, 200, map[string]interface{}{"ok": true, "detail": "没有 browser 来源账号", "n": 0})
		return
	}
	nOK, nFail := 0, 0
	var errs []string
	for _, a := range accts {
		ok, _, err := browserRefreshOne(a.Label, a.Profile)
		if err != nil {
			nFail++
			errs = append(errs, fmt.Sprintf("%s: %v", a.Profile, err))
			continue
		}
		_ = ok
		nOK++
		markAccountResult(a.ID, true, "")
	}
	detail := fmt.Sprintf("刷新完成：成功 %d，失败 %d", nOK, nFail)
	if len(errs) > 0 {
		detail += " · " + strings.Join(errs, "；")
	}
	writeJSON(w, 200, map[string]interface{}{"ok": nFail == 0, "detail": detail, "ok_count": nOK, "fail_count": nFail})
}

// handleAdminBrowserEnv — GET /admin/api/browser/env
//
// 部署环境识别，供前端「初始化引导」决定走哪条路：
//   - in_docker:     本进程是否跑在容器里
//   - os:            运行平台（linux/windows/darwin）
//   - has_controller: 是否配置了浏览器控制器（容器/同机 Chromium 方案）
//   - has_chromium:  控制器在线且至少有一个 profile 在跑
//   - browser_kind:  推断出的浏览器方案：docker-chromium / local-chromium / remote-extension / none
//
// 远程方案（用户自己电脑上的 Chrome/Edge + 扩展）无法从服务端探测 —— 那部分
// 由前端在浏览器里判断（navigator 信息），服务端只提供扩展下载与接入说明。
func handleAdminBrowserEnv(w http.ResponseWriter, r *http.Request) {
	inDocker := false
	if _, err := os.Stat("/.dockerenv"); err == nil {
		inDocker = true
	}
	hasController := browserEnabled()
	controllerHealthy := false
	runningProfiles := 0
	if hasController {
		var ok bool
		var errStr string
		ok, runningProfiles, errStr = browserHealth()
		controllerHealthy = ok
		_ = errStr
	}

	kind := "none"
	switch {
	case hasController && controllerHealthy && runningProfiles > 0:
		kind = "docker-chromium"
	case hasController && controllerHealthy:
		kind = "docker-chromium" // 控制器在、还没建 profile，也算走这条方案
	case hasController:
		kind = "docker-chromium" // 配了但离线，仍按容器方案引导（会提示修复）
	default:
		kind = "remote-extension" // 没配控制器 → 推荐用用户自己电脑的浏览器 + 扩展
	}

	writeJSON(w, 200, map[string]interface{}{
		"in_docker":         inDocker,
		"os":                runtime.GOOS,
		"has_controller":    hasController,
		"controller_ok":     controllerHealthy,
		"running_profiles":  runningProfiles,
		"browser_kind":      kind,
		"controller_url":    browserControllerURL(),
		"access_url":        browserAccessURL(),
		"extension_ready":   true, // 扩展源码内嵌在本二进制里，见 handleAdminBrowserExtension
		"pool_has_cookie":   hasCookie(),
		"guide_completed":   kvGet(kvBrowserGuideDone) == "1",
	})
}

// kvBrowserGuideDone 记录「初始化引导已完成」。
const kvBrowserGuideDone = "browser_guide_completed"

// handleAdminBrowserGuideDone — POST /admin/api/browser/guide-done 标记引导完成
func handleAdminBrowserGuideDone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	_ = kvSet(kvBrowserGuideDone, "1")
	writeJSON(w, 200, map[string]interface{}{"ok": true})
}

// handleAdminBrowserExtension — GET /admin/api/browser/extension
//
// 把内嵌的浏览器扩展打包成 zip 下载，供「用你自己电脑上的浏览器抓取」场景
// 安装（Chrome/Edge 开发者模式加载解压目录）。扩展源码来自 tools/cookie-sync/ext-src，
// 通过 go:embed 打进二进制（见 ext_embed.go）。
func handleAdminBrowserExtension(w http.ResponseWriter, r *http.Request) {
	if !extEmbedded() {
		writeJSON(w, 404, map[string]string{"error": "扩展未内嵌进本构建"})
		return
	}
	data, err := buildExtZip()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="gw2a-cookie-sync-extension.zip"`)
	_, _ = w.Write(data)
}
