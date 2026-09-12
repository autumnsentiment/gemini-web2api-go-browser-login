package app

import (
	"encoding/json"
	"strings"
)

// Source 是一条联网搜索引用的来源。
type Source struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

// extractGrounding 从 StreamGenerate 原始响应里抠出联网搜索的来源。
//
// 位置：帧的 inner[4][0][2][1] 是一组 grounding chunk，每个 chunk 的 [2] 是来源列表，
// 每条来源形如 [url, title, favicon, snippet]。逐字取自抓包，跟 textsInLine 走同一条
// wrb.fr → inner 的解析路径。没有联网搜索时该结构不存在，返回 nil。
//
// URL 常带 `#:~:text=` 文字片段锚（Google 搜索的"滚动到指定文字"），展示时截掉更干净。
func extractGrounding(raw string) []Source {
	var out []Source
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, `"wrb.fr"`) || len(line) < 200 {
			continue
		}
		var arr []interface{}
		if json.Unmarshal([]byte(line), &arr) != nil || len(arr) == 0 {
			continue
		}
		first, ok := arr[0].([]interface{})
		if !ok || len(first) < 3 {
			continue
		}
		innerStr, ok := first[2].(string)
		if !ok {
			continue
		}
		var inner []interface{}
		if json.Unmarshal([]byte(innerStr), &inner) != nil || len(inner) <= 4 {
			continue
		}
		chunks := groundingChunks(inner)
		for _, c := range chunks {
			cl, ok := c.([]interface{})
			if !ok || len(cl) <= 2 {
				continue
			}
			srcs, ok := cl[2].([]interface{})
			if !ok {
				continue
			}
			for _, s := range srcs {
				sl, ok := s.([]interface{})
				if !ok || len(sl) < 2 {
					continue
				}
				url, _ := sl[0].(string)
				url = stripTextFragment(url)
				if url == "" || seen[url] {
					continue
				}
				title, _ := sl[1].(string)
				snippet := ""
				if len(sl) > 3 {
					snippet, _ = sl[3].(string)
				}
				seen[url] = true
				out = append(out, Source{URL: url, Title: title, Snippet: snippet})
			}
		}
	}
	return out
}

// groundingChunks 取 inner[4][0][2][1]，任何一层缺失都返回 nil。
func groundingChunks(inner []interface{}) []interface{} {
	f4, ok := inner[4].([]interface{})
	if !ok || len(f4) == 0 {
		return nil
	}
	node, ok := f4[0].([]interface{})
	if !ok || len(node) <= 2 {
		return nil
	}
	g, ok := node[2].([]interface{})
	if !ok || len(g) <= 1 {
		return nil
	}
	chunks, ok := g[1].([]interface{})
	if !ok {
		return nil
	}
	return chunks
}

// stripTextFragment 去掉 URL 尾部的 `#:~:text=...` 片段锚。
func stripTextFragment(u string) string {
	if i := strings.Index(u, "#:~:text="); i >= 0 {
		return u[:i]
	}
	return u
}
