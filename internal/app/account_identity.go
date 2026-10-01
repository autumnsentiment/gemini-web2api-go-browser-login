package app

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Gemini 页面在 WIZ_global_data 里写着当前会话属于哪个 Google 账号：
// oPEP7c 是邮箱，QrtxK 是会话槽位（/u/N 的 N）。同一份 cookie 访问
// /u/N/app，返回的就是第 N 个已登录账号；槽位不存在时 Google 302 回 /u/0。
//
// 扩展只能从浏览器读到整份共享的 cookie jar，决定账号的是槽位。所以
// 入池时服务端用这份 cookie 实际打开一次 /u/N/app，确认页面上的账号
// 就是扩展绑定页显示的那个邮箱，不一致直接拒收。这样 URL 不带 /u/N 的
// 页面、或是旧扩展猜错槽位，都不会把别的账号写进池子。
var (
	pageEmailRe = regexp.MustCompile(`"oPEP7c":"([^"@]+@[^"]+)"`)
	pageSlotRe  = regexp.MustCompile(`"QrtxK":"(\d)"`)
)

// pageIdentity 是 /u/N/app 页面自报的账号身份。
type pageIdentity struct {
	Email    string
	Slot     int // -1 = 页面没给
	LoggedIn bool
}

func parsePageIdentity(body []byte) pageIdentity {
	id := pageIdentity{Slot: -1}
	id.LoggedIn = snlm0eRe.Find(body) != nil
	if m := pageEmailRe.FindSubmatch(body); m != nil {
		id.Email = strings.ToLower(strings.TrimSpace(string(m[1])))
	}
	if m := pageSlotRe.FindSubmatch(body); m != nil {
		if n, err := strconv.Atoi(string(m[1])); err == nil {
			id.Slot = n
		}
	}
	return id
}

// verifySlotIdentity 用 cookie 打开 /u/<authuser>/app，确认页面账号是 wantEmail。
// wantEmail 为空时只校验该槽位确实登录着（旧扩展没上报邮箱）。
func verifySlotIdentity(cookie string, authuser int, wantEmail string) (pageIdentity, error) {
	picked, ok, err := acquireSlot(0)
	if !ok {
		if err == nil {
			err = fmt.Errorf("no usable proxy")
		}
		return pageIdentity{Slot: -1}, fmt.Errorf("拿不到出口，无法复核账号：%v", err)
	}
	defer releaseSlot(picked.ID)
	body, err := fetchAppPage(cookie, picked.URL, authuser)
	if err != nil {
		// 不存在的槽位：Google 把 /u/N 302 回默认账号，这里表现为 HTTP 302。
		return pageIdentity{Slot: -1}, fmt.Errorf("槽位 %s 打不开（%v），该浏览器可能没有登录这个账号", authUserLabel(authuser), err)
	}
	return checkSlotIdentity(parsePageIdentity(body), authuser, wantEmail)
}

func checkSlotIdentity(id pageIdentity, authuser int, wantEmail string) (pageIdentity, error) {
	if !id.LoggedIn {
		return id, fmt.Errorf("槽位 %s 页面未登录（没有会话令牌）", authUserLabel(authuser))
	}
	if id.Slot >= 0 && id.Slot != authuser {
		return id, fmt.Errorf("请求槽位 %s，页面却是槽位 %s", authUserLabel(authuser), authUserLabel(id.Slot))
	}
	want := strings.ToLower(strings.TrimSpace(wantEmail))
	if want != "" && id.Email != "" && id.Email != want {
		return id, fmt.Errorf("槽位 %s 实际是 %s，与扩展绑定页的 %s 不一致", authUserLabel(authuser), id.Email, want)
	}
	if want != "" && id.Email == "" {
		return id, fmt.Errorf("槽位 %s 页面没有返回账号邮箱，无法确认身份", authUserLabel(authuser))
	}
	return id, nil
}

// purgeLegacySlotProfiles 清掉旧版多账号扩展留下的 profile-uN 残留：
// 池记录、扩展心跳 kv、抓取排程 kv。旧扩展把每个 /u/N 槽位伪装成
// 独立 profile（browser1-u1…u9），新版入口已拒收，这里只收拾存量。
func purgeLegacySlotProfiles() {
	db := getDB()
	type row struct {
		id      int64
		profile string
	}
	var accts []row
	if rows, err := db.Query(`SELECT id, profile FROM accounts WHERE source IN ('browser','remote') AND profile LIKE '%-u%'`); err == nil {
		for rows.Next() {
			var r row
			if rows.Scan(&r.id, &r.profile) == nil && isLegacyMultiAccountProfile(r.profile) {
				accts = append(accts, r)
			}
		}
		rows.Close()
	}
	for _, r := range accts {
		if _, err := db.Exec(`DELETE FROM accounts WHERE id=?`, r.id); err == nil {
			logf("[cleanup] 删除旧版多账号扩展遗留账号 #%d（profile %q）", r.id, r.profile)
		}
	}
	var keys []string
	if rows, err := db.Query(`SELECT k FROM kv WHERE k LIKE '%-u%'`); err == nil {
		for rows.Next() {
			var k string
			if rows.Scan(&k) != nil {
				continue
			}
			i := strings.Index(k, ":")
			if i <= 0 {
				continue
			}
			prefix := k[:i+1]
			if !strings.HasPrefix(prefix, "ext_") && prefix != "browser_next_refresh:" {
				continue
			}
			if isLegacyMultiAccountProfile(k[i+1:]) {
				keys = append(keys, k)
			}
		}
		rows.Close()
	}
	for _, k := range keys {
		_, _ = db.Exec(`DELETE FROM kv WHERE k=?`, k)
	}
	if len(keys) > 0 {
		logf("[cleanup] 清理旧版多账号扩展遗留状态 %d 条（ext_* / browser_next_refresh）", len(keys))
	}
}
