# 来源声明 / Attribution

本项目是 **[zexadev/gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go)**
的**浏览器登录拓展版**：完整保留原项目的全部功能与协议实现，在其之上扩展出
「浏览器登录 → 自动抓取 Cookie → 自动保活」的完整链路。

## 上游

| 项 | 内容 |
|---|---|
| 项目 | gemini-web2api-go |
| 仓库 | https://github.com/zexadev/gemini-web2api-go |
| 基线版本 | v4.20.0（本仓库已同步至该版本） |
| 许可证 | MIT License |
| 版权 | Copyright (c) 2026 gemini-web2api-go contributors |

本仓库中的以下部分来自上游，版权归上游所有，按 MIT 许可证使用：

- `main.go`
- `internal/app/**`（Go 后端基础代码，除下文列出的新增/修改文件）
- `Dockerfile`、`go.mod`、`go.sum`、`config.example.json`
- `.github/workflows/**`
- `docs/UPSTREAM_README.md`、`docs/UPSTREAM_README_EN.md`、`docs/UPSTREAM_CHANGELOG.md`、`docs/banner.svg`、`docs/banner_gen.py`

## 本拓展版新增 / 修改

以下部分为本项目原创，同样以 MIT 许可证发布：

**Go 后端（新增）**

- `internal/app/browser_cdp.go` —— CDP 浏览器登录态获取（只读抓取 / 失效才刷新 / 按有效期调度）
- `internal/app/browser_verify.go` —— 抓取后模型校验、302 重置代理池自愈
- `internal/app/browser_ingest.go` —— 远程浏览器扩展推送 cookie 的接收端点
- `internal/app/admin_browser.go` —— 浏览器相关管理接口（含环境识别与引导）
- `internal/app/ext_embed.go`、`internal/app/ext_assets/` —— 抓取扩展内嵌与打包下载
- `internal/app/model_guard.go` —— 模型一致性检测（上游静默降级时自动重抓）
- `internal/app/images.go`、`internal/app/images_test.go` —— `/v1/images/*` OpenAI 图像 API

**Go 后端（修改）**

- `internal/app/xsrf.go` —— 新增 `xsrfAuthStatus()` 错误分类
- `internal/app/gemini.go` —— 错误分类替代硬编码 401；媒体请求独立超时与重试策略
- `internal/app/cookie_pool.go` —— 网络错误不计入 `fail_count`；续票→浏览器刷新自愈链
- `internal/app/client.go` —— 按超时缓存独立客户端实例（媒体请求长超时）
- `internal/app/app.go`、`internal/app/config.go`、`internal/app/runtime.go`、`internal/app/admin_cookies.go`、`internal/app/bl.go`、`internal/app/db.go`、`internal/app/admin_ui/index.html` —— 浏览器登录相关接线与面板

**配套工具链（全部新增）**

- `tools/browser-controller/` —— Chromium profile 控制器（Node.js）
- `tools/cookie-sync/` —— Cookie 抓取工具链（Python 3）+ MV3 扩展源码
- `tools/image-gen/` —— 图片生成前端插件 + nginx 反代
- `deploy/` —— 生产用 compose、本地构建 Dockerfile、systemd 单元

## 上游致谢

感谢 [@zexadev](https://github.com/zexadev) 及 gemini-web2api-go 的所有贡献者。
没有上游扎实的协议逆向工作，本拓展版的浏览器登录自动化无从谈起。
