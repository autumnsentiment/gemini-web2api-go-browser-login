package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
//	POST   /admin/api/browser/profiles/<name>/fetch    立即抓 cookie 入库
//	DELETE /admin/api/browser/profiles/<name>          停实例 + 删对应池记录
//
// profile 名就是「账号隔离单元」：每个 profile 对应 chromium 容器里一个独立
// Chromium user-data-dir + CDP 端口，登录态互不相通。

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
		"access_url": strings.TrimSpace(cfg.BrowserAccessURL),
	})
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
			// 立即抓 cookie 入库
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
			// 抓成功后返回账号 id
			nacct, _ := findBrowserAccountByProfile(name)
			id := int64(0)
			if nacct != nil {
				id = nacct.ID
			}
			writeJSON(w, 200, map[string]interface{}{
				"ok": ok, "account_id": id,
				"detail": fmt.Sprintf("cookie 已抓取并写入 cookie 池（账号 #%d）", id),
			})
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
