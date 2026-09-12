package app

import (
	"regexp"
	"sync"
	"time"
)

// bl 是上游前端的构建版本号，形如 boq_assistant-bard-web-server_20260805.16_p0，
// 每个 StreamGenerate 请求都要带。它埋在 /app 页面 HTML 的 "cfb2h" 字段里。
//
// 钉死一个值是能用的 —— 我们钉的 20260525.09_p0 比抓包里浏览器用的
// 20260805.16_p0 落后两个多月，实测仍然正常。但"仍然可用"不等于"永远可用"，
// 而这个值一旦被上游弃用，表现是所有请求大面积失败，排查时很难第一时间想到
// 是个版本号过期了。既然 xsrf.go 本来就在抓 /app 页面，顺手取一下成本几乎为零。
var (
	blMu       sync.RWMutex
	blFetched  string    // 抓到的最新值，空表示还没抓到
	blFetchAt  time.Time // 上次抓取尝试的时间（成功失败都记，避免失败时疯狂重试）
	blFetching bool      // 有一次抓取正在飞
)

// bl 变化的粒度是天，隔几小时看一眼足够。
const blTTL = 6 * time.Hour

var cfb2hRe = regexp.MustCompile(`"cfb2h":"([^"]{10,120})"`)

// blValueRe 卡住形状：boq_assistant-bard-web-server_<8位日期>.<2位>_p<数字>。
// 页面上的 cfb2h 理论上就是这个格式，但它是外部输入，直接拿去拼 URL 等于让
// 上游页面决定我们发什么请求。形状对不上就当没抓到，继续用配置里钉的值 ——
// 宁可落后，不可乱发。
var blValueRe = regexp.MustCompile(`^boq_assistant-bard-web-server_\d{8}\.\d{2}_p\d+$`)

// currentBL 返回这次请求该用的 bl。
//
// 不阻塞：过期时只是踢一次后台抓取，本次仍返回当前值。bl 是缓慢变化的版本号，
// 用旧值最多是落后几个版本（实测落后两个多月仍可用），不值得为它给每个请求
// 加一次页面抓取的延迟。auto 关掉时永远用配置里的值。
func currentBL(proxyURL string) string {
	pinned := rtCfg().GeminiBL
	if !rtCfg().GeminiBLAuto {
		return pinned
	}

	blMu.RLock()
	val, fetchedAt, inflight := blFetched, blFetchAt, blFetching
	blMu.RUnlock()

	if !inflight && time.Since(fetchedAt) > blTTL {
		blMu.Lock()
		// 双检：并发请求下只放一个 goroutine 去抓。
		if !blFetching && time.Since(blFetchAt) > blTTL {
			blFetching = true
			go refreshBL(proxyURL)
		}
		blMu.Unlock()
	}

	if val != "" {
		return val
	}
	return pinned
}

// currentBLPinned 返回配置里钉死的 bl，**不做自动更新**。
//
// 媒体请求（图片/音乐/视频）必须用它。实测（2026-09-12）：自动抓到的
// boq_assistant-bard-web-server_20260910.05_p2 会让 inner[49] 的工具位失效 ——
// 上游不理这个字段、退回纯文本模型（响应自带模型标 "3.6 Flash"），于是
// "media generation succeeded but artifact retrieval failed: 响应里没有图片 CDN 链接"。
// 同一账号、同一代理、同一份请求体，只把 bl 换回钉死的 20260525.09_p0 就立刻恢复
// （响应标回 "Nano Banana 2"）。工具位是上游前端私有的编码，新版前端改了含义，
// 我们钉的值才是跟当前请求体匹配的那一版。
//
// 所以媒体走钉死值、普通对话走自动值：对话的载荷简单，新旧 bl 都能用；
// 工具的载荷是逆向出来的，必须配对。
func currentBLPinned() string {
	return rtCfg().GeminiBL
}

// refreshBL 抓一次 /app 把 cfb2h 取出来。匿名抓即可，这个值跟登录态无关。
func refreshBL(proxyURL string) {
	defer func() {
		blMu.Lock()
		blFetching = false
		blFetchAt = time.Now() // 成功失败都记，失败时也要等满 TTL 再试
		blMu.Unlock()
	}()

	body, err := fetchAppPage("", proxyURL)
	if err != nil {
		logf("[bl] 抓 /app 失败，继续用当前值: %v", err)
		return
	}
	m := cfb2hRe.FindSubmatch(body)
	if m == nil {
		logf("[bl] 页面里没有 cfb2h，继续用当前值")
		return
	}
	got := string(m[1])
	if !blValueRe.MatchString(got) {
		logf("[bl] 抓到的值形状不对，忽略: %q", truncate(got, 60))
		return
	}

	blMu.Lock()
	changed := blFetched != got
	blFetched = got
	blMu.Unlock()
	if changed {
		logf("[bl] 更新为 %s（配置里钉的是 %s）", got, rtCfg().GeminiBL)
	}
}
