# 来源声明 / Attribution

本项目是 **[zexadev/gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go)** 的**派生增强版（derivative work）**。

## 上游

| 项 | 内容 |
|---|---|
| 项目 | gemini-web2api-go |
| 仓库 | https://github.com/zexadev/gemini-web2api-go |
| 基线版本 | v4.17.0 |
| 许可证 | MIT License |
| 版权 | Copyright (c) 2026 gemini-web2api-go contributors |

本仓库中的以下部分直接来自上游，版权归上游所有，按 MIT 许可证使用：

- `main.go`
- `internal/app/**`（Go 后端全部代码，除下文列出的新增/修改文件）
- `Dockerfile`、`go.mod`、`go.sum`、`config.example.json`
- `.github/workflows/**`
- `docs/UPSTREAM_README.md`、`docs/UPSTREAM_README_EN.md`、`docs/UPSTREAM_CHANGELOG.md`、`docs/banner.svg`、`docs/banner_gen.py`

## 本项目新增 / 修改

以下部分为本项目原创，同样以 MIT 许可证发布：

**Go 后端（新增）**

- `internal/app/browser_cdp.go` —— CDP 浏览器登录态获取
- `internal/app/admin_browser.go` —— 浏览器相关管理接口
- `internal/app/images.go`、`internal/app/images_test.go` —— `/v1/images/*` OpenAI 图像 API

**Go 后端（修改）**

- `internal/app/xsrf.go` —— 新增 `xsrfAuthStatus()` 错误分类
- `internal/app/gemini.go` —— 用错误分类替代硬编码 401
- `internal/app/cookie_pool.go` —— 网络错误不计入 `fail_count`
- `internal/app/app.go`、`internal/app/config.go`、`internal/app/runtime.go`、`internal/app/admin_cookies.go`、`internal/app/bl.go`、`internal/app/db.go`、`internal/app/admin_ui/index.html` —— 浏览器登录相关接线

**配套工具链（全部新增）**

- `tools/browser-controller/` —— Chromium profile 控制器（Node.js）
- `tools/cookie-sync/` —— Cookie 抓取工具链（Python 3）+ MV3 扩展源码
- `tools/image-gen/` —— 图片生成前端插件 + nginx 反代
- `deploy/` —— 生产用 compose、本地构建 Dockerfile、systemd 单元

## 上游致谢

感谢 [@zexadev](https://github.com/zexadev) 及 gemini-web2api-go 的所有贡献者。
没有上游扎实的协议逆向工作，本项目的浏览器登录自动化无从谈起。
