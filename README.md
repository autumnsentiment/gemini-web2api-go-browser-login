<div align="center">

# gemini-web2api-go · 浏览器登录增强版

**把 Google Gemini 网页端反代成 OpenAI 兼容 API —— 带全自动 Cookie 供给链路**

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)](https://go.dev)
[![Upstream](https://img.shields.io/badge/upstream-zexadev%2Fgemini--web2api--go-181717.svg)](https://github.com/zexadev/gemini-web2api-go)

</div>

---

## 源项目声明（必读）

本项目是 **[zexadev/gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go)**（MIT License）的**派生增强版**。

| | |
|---|---|
| **上游项目** | <https://github.com/zexadev/gemini-web2api-go> |
| **上游版本基线** | `v4.17.0`（`internal/app/server.go` 中 `Version = "4.17.0"`） |
| **上游许可证** | MIT License, Copyright (c) 2026 gemini-web2api-go contributors |
| **本仓库** | 在上游基础上增加了「浏览器登录态自动抓取链路」「OpenAI 图像 API 端点」「图片生成前端插件」三块自研扩展，并修复若干 Cookie 池健壮性问题 |

上游的完整文档、更新日志已随仓库保留，见：

- [`docs/UPSTREAM_README.md`](docs/UPSTREAM_README.md) —— 上游完整中文文档（模型清单、接口矩阵、指纹说明、面板用法等）
- [`docs/UPSTREAM_README_EN.md`](docs/UPSTREAM_README_EN.md) —— 上游英文文档
- [`docs/UPSTREAM_CHANGELOG.md`](docs/UPSTREAM_CHANGELOG.md) —— 上游更新日志

**上游项目是主体，本项目只做增量。** 如果你只需要基础的 Gemini → OpenAI 反代，直接用上游即可；本仓库的价值在于把「登录态获取」这件事做成无人值守的闭环。

---

## 本项目相对上游新增了什么

### 1. 全自动 Cookie 供给链路（核心）

上游需要你**手动**把 Google 的 cookie 粘进管理面板。cookie 会过期（`__Secure-1PSIDTS` 通常几十分钟到几小时轮换一次），过期后所有请求 502，必须人工重新粘贴 —— 这是无人值守场景最大的痛点。

本项目在服务器自带的 Chromium 上常驻一个已登录的 Google 会话，用**扩展**读取真实会话 cookie，自动写入 gemini-web2api 的 cookie 池：

```
┌─────────────────────────── 宿主机（systemd）───────────────────────────┐
│                                                                       │
│  gw2a-xkasmvnc.service     Xkasmvnc 虚拟桌面 :13（VNC Web :16100）     │
│           │                                                           │
│  gw2a-browser-controller   拉起 Chromium（CDP 9300+），提供 :9280     │
│           │                 /profiles /cookie-sync /cdp/<port>/*      │
│           │                                                           │
│      Chromium（profile: acct1，已登录 Google）                        │
│           │                                                           │
│      ext-src 扩展「Gemini Cookie Sync」                               │
│           │  刷新页面 → 冷却窗口内读 cookie → POST /cookie-sync       │
│           ▼                                                           │
│  gw2a-cookie-queue.service  队列守护：30min 定时 + 2×502 触发重抓     │
│           │                 串行执行、浏览器保活、502 后重新计时      │
│           ▼                                                           │
│  refresh.py → pool.py → 直接写 SQLite（WAL 安全）                     │
└───────────────────────────────────┬───────────────────────────────────┘
                                    ▼
                    gemini-web2api 容器的 cookie 池
```

关键设计：

- **不新开页面，只刷新已有页面** —— 早期实现每次新建 Gemini 会话，很快被风控。现在改为刷新现有标签页。
- **60 秒冷却窗口** —— 刷新页面后 60s 内提取 cookie；窗口内提取失败才再刷新，避免高频操作。
- **队列串行 + 重新计时** —— 定时任务与 502 触发的重抓不会并发；502 触发后定时器重新计时。
- **2×502 立即重抓** —— 轮询 `requests` 表，出现两个 502 立刻触发一次 cookie 刷新。
- **10 分钟保活** —— 定期刷新页面维持会话活跃，防止 Google 因长期不用而要求重新验证。
- **出口绑定** —— 账号 `proxy_id` 自动绑定为浏览器所用出口。若账号走直连而浏览器走代理，出口 IP 不一致，Google 会立刻判会话可疑并失效。

### 2. OpenAI 图像 API 端点

上游只认 chat 端点发图。很多客户端（new-api / Cherry Studio / LobeChat 的画图页）只用 OpenAI 图像 API，导致 `gemini-image` 在它们那里根本选不到。

新增 `internal/app/images.go`：

| 端点 | 说明 |
|---|---|
| `POST /v1/images/generations` | JSON；带 `image`（data URL / http 链接）即为图生图。结果 `data[].b64_json` |
| `POST /v1/images/edits` | multipart：`image`（可重复）/ `mask` / `prompt` / `model` / `n` / `size` / `response_format` |

两者都收敛到同一条 `gemini-image` 链路（上游 `StreamGenerate` + `inner[49]=14`），产物由 `hNvQHb` 取回原始字节后转成 OpenAI images 形状。

还包含**回声检测**（`isEchoArtifacts`）：图生图时上游偶尔丢掉生图标记、把输入图原样送回，此时按近似像素比对识别并丢弃重打，避免客户端拿到一张和原图一模一样的「修改后」图片。

### 3. 图片生成前端插件

`tools/image-gen/` 是一个独立的生图工作台页面（`image-gen.html`）+ nginx 反代，把 `/v1/images/edits` 请求转换到 `/v1/chat/completions`，并支持本地图库（IndexedDB）。图库参考图入队前会在浏览器侧压缩，避免大图触发网关 413。

### 4. Cookie 池健壮性修复

| 文件 | 修复 |
|---|---|
| `internal/app/gemini.go` | `markCookieByStatus(acct.ID, xsrfAuthStatus(err), ...)` —— 不再把所有错误硬编码成 401 |
| `internal/app/xsrf.go` | 新增 `xsrfAuthStatus()`，把错误分类成 401/403/5xx/网络错误 |
| `internal/app/browser_cdp.go` | 新增 `errBrowserPageUnreachable` / `errBrowserNotLoggedIn` 哨兵错误，网络不可达不再误删账号 |
| `internal/app/cookie_pool.go` | 检测路径不再把网络错误计入 `fail_count`，避免好 cookie 被代理故障误伤成「失败最多」 |

上游行为：代理一挂，所有请求报错 → cookie 被标 401 → `fail_count` 累加 → 账号被自动停用 → 代理恢复后还得手动启用。修复后网络错误不计入健康度，只有确凿的 401/403 才降级。

### 5. 生图会话删除独立开关

上游只有一个「自动删网页会话」开关，对对话和生图一视同仁。但两者的诉求正好相反：

- **对话**会话是「过程」，留着只会在账号里堆垃圾 → 倾向删
- **生图**会话是「资产」，用户常想回网页端复看或二次编辑 → 倾向留

所以本项目把它拆成两个独立开关：

| 面板选项 | JSON 字段 | 作用范围 |
|---|---|---|
| 自动删网页会话（对话） | `auto_delete_conversation` | 对话 / 音乐 / 视频 / 画布 |
| 自动删网页会话（生图） | `auto_delete_image_conversation` | 仅 `gemini-image` |

两者完全独立，互不影响。实现在 `internal/app/gemini.go`：

```go
// autoDeleteForTool 决定某个模型类型出完结果后要不要自动删网页会话。
func autoDeleteForTool(tool int) bool {
	rt := rtCfg()
	if tool == toolImage {
		return rt.AutoDeleteImageConversation
	}
	return rt.AutoDeleteConversation
}
```

两个开关默认都是 `false`（与「不删会话」的上游默认行为一致）。

由于 `/v1/images/*` 与 `/v1/chat/completions` 最终都收敛到 `callGemini`，删除点只有一处，
按 `mc.Tool` 分流即可，不会出现两条路径判断不一致的情况。

实测验证矩阵（真实打上游：每个 case 单独配置后发请求，按时间戳统计 `[autodel]` 日志行）：

| 请求类型 | 对话开关 | 生图开关 | 实际删除 | 预期 |
|---|---|---|---|---|
| 生图 | 关 | 关 | 0 | 0 ✅ |
| 生图 | 关 | **开** | 1 | ≥1 ✅ |
| 生图 | **开** | 关 | 0 | 0 ✅（对话开关未泄漏到生图）|
| 对话 | **开** | 关 | 1 | ≥1 ✅ |
| 对话 | 关 | **开** | 0 | 0 ✅（生图开关未泄漏到对话）|
| 对话 | 关 | 关 | 0 | 0 ✅ |

6/6 全部通过。

---

## 目录结构

```
.
├── main.go                     # 上游入口（未改动）
├── internal/app/               # 上游 Go 源码 + 本项目新增/修改
│   ├── browser_cdp.go          # [新增] CDP 浏览器登录态获取
│   ├── admin_browser.go        # [新增] 浏览器相关管理接口
│   ├── images.go               # [新增] /v1/images/* OpenAI 图像 API
│   ├── xsrf.go                 # [修改] xsrfAuthStatus() 错误分类
│   ├── gemini.go               # [修改] 错误分类回写
│   └── cookie_pool.go          # [修改] 网络错误不计入 fail_count
│
├── tools/
│   ├── browser-controller/     # Chromium profile 控制器（Node.js）
│   │   ├── controller.js       #   拉起 Chromium、CDP 隧道、/cookie-sync 接口
│   │   └── vnc-proxy.js        #   VNC 路径反代
│   ├── cookie-sync/            # Cookie 抓取工具链（Python 3）
│   │   ├── refresh.py          #   页面刷新式抓取 + 60s 冷却窗口（核心）
│   │   ├── queue_daemon.py     #   30min 定时 + 2×502 触发、队列串行
│   │   ├── pool.py             #   直接写容器共享 SQLite（WAL 安全）
│   │   ├── watchdog.py         #   每 5 分钟兜底
│   │   ├── gw2a-sync.py        #   命令行：state/status/keepalive/sync/pool/open
│   │   ├── install_ext.py      #   打包并安装扩展
│   │   └── ext-src/            #   扩展源码（MV3）
│   └── image-gen/              # 图片生成前端插件 + nginx 反代
│
├── deploy/
│   ├── docker-compose.yml      # 生产用 compose（含浏览器登录环境变量）
│   ├── build/                  # 本地构建镜像的 Dockerfile + build.sh
│   └── systemd/                # 全部 systemd 单元（8 个）
│
└── docs/                       # 上游原始文档（保留出处）
```

---

## 使用方法

### 基础用法（与上游一致）

启动后，把任意 OpenAI 客户端指向 `http://<host>:8083/v1`：

```bash
curl http://localhost:8083/v1/chat/completions \
  -H "Authorization: Bearer <你的 API Key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-3.6-flash","messages":[{"role":"user","content":"你好"}]}'
```

常用模型（完整清单见上游文档）：

| 模型 | 说明 |
|---|---|
| `gemini-3.6-flash` | 匿名可用 |
| `gemini-3.5-flash-lite` | 匿名可用 |
| `gemini-3.6-flash-thinking` | **需要 cookie** |
| `gemini-3.1-pro-thinking` | **需要 cookie** |
| `gemini-image` | 生图（Nano Banana），**需要 cookie** |
| `gemini-music` | 音乐（Lyria），**需要 cookie** |

管理面板：`http://<host>:8083/`（用 `ADMIN_TOKEN` 登录）。

### 生图（本项目扩展）

```bash
# 文生图
curl http://localhost:8083/v1/images/generations \
  -H "Authorization: Bearer <key>" -H "Content-Type: application/json" \
  -d '{"model":"gemini-image","prompt":"一只在月球上喝咖啡的猫"}'

# 图生图（multipart）
curl http://localhost:8083/v1/images/edits \
  -H "Authorization: Bearer <key>" \
  -F "model=gemini-image" -F "prompt=把背景换成雪山" \
  -F "image=@input.png"
```

### 接入 new-api

在 new-api 里新建渠道，类型选 **OpenAI**，Base URL 填 `http://<gemini-web2api 地址>:8083`，填入 API Key。之后 new-api 的 `/v1/images/edits` 会被本项目转换成 chat 请求发往 Gemini 网页端，生成的图片以 `b64_json` 回传到下游。

---

## 部署方法

### 前置条件

| 组件 | 版本 | 说明 |
|---|---|---|
| Go | 1.26+ | 仅源码构建需要 |
| Node.js | 18+ | 浏览器控制器 |
| Python | 3.11+ | cookie-sync 工具链 |
| Chromium | 任意较新版本 | 需能跑在有 GUI（或 Xkasmvnc）的环境 |
| 出口代理 | 可选但**强烈建议** | 见下方「网络」说明 |

> 本项目在 **fnOS** 上开发和验证，Chromium 用的是 fnOS 自带 `fygo-browser` 的运行时。其他系统请把 `controller.js` 里的 `RT` / `CHROMIUM` / `BROWSER_HOME` 等路径改成你自己的。

### 方式一：Docker（推荐）

```bash
# 1) 构建镜像
cd deploy/build
#   先按需修改 build.sh 里的挂载路径
./build.sh
docker build -t gemini-web2api:local-browser -f Dockerfile .

# 2) 启动
cd ..
export ADMIN_TOKEN='你的面板 token'
docker compose up -d
```

`deploy/docker-compose.yml` 已经带上了浏览器登录相关环境变量：

```yaml
environment:
  ADMIN_TOKEN: "${ADMIN_TOKEN:-change-me-in-production}"
  BROWSER_CONTROLLER_URL: "http://172.21.0.1:9280"   # 宿主机网关 + 控制器端口
  BROWSER_CDP_HOST: "172.21.0.1"
  BROWSER_ACCESS_URL: "http://<你的NAS地址>:16100/"   # VNC 桌面入口
```

> `172.21.0.1` 是 compose 网络 `gemini-web2api-go_default` 的宿主网关，**不是固定值**，请用
> `docker network inspect gemini-web2api-go_default -f '{{(index .IPAM.Config 0).Gateway}}'` 查出来再填。

不用浏览器登录链路的话，把上面三个 `BROWSER_*` 删掉即可，行为与上游一致。

### 方式二：单二进制

```bash
go build -trimpath -ldflags="-s -w" -o gemini-web2api .
./gemini-web2api --db ./data/gemini.db --port 8083 \
                 --admin-token <token> \
                 --browser-controller-url http://127.0.0.1:9280 \
                 --browser-cdp-host 127.0.0.1 \
                 --browser-access-url http://<你的NAS地址>:16100/
```

### 部署 Cookie 自动供给链路

以下假设部署到 `/opt/gw2a-cookie-sync` 与 `/opt/gw2a-browser-controller`（systemd 单元里写死了这两个路径，换路径需同步改）。

**1) 拷贝工具链**

```bash
sudo mkdir -p /opt/gw2a-cookie-sync /opt/gw2a-browser-controller
sudo cp -r tools/cookie-sync/*        /opt/gw2a-cookie-sync/
sudo cp    tools/browser-controller/* /opt/gw2a-browser-controller/
sudo chmod +x /opt/gw2a-browser-controller/controller.js
```

**2) 装 Python 依赖**

```bash
python3 -m venv /opt/gw2a-cookie-sync/venv
/opt/gw2a-cookie-sync/venv/bin/pip install websocket-client
```

**3) 建状态目录**

```bash
sudo mkdir -p /var/lib/gw2a-cookie-sync
```

**4) 改 systemd 单元里的占位符**

`deploy/systemd/*.service` 里有三处必须改：

| 占位符 | 改成 |
|---|---|
| `CHANGE_ME_ADMIN_TOKEN` | 与 gemini-web2api 的 `ADMIN_TOKEN` 一致 |
| `http://127.0.0.1:7890` | 你的出口代理地址（不用代理就删掉这行） |
| `/vol2/@appdata/gw2a-browser/profiles` | 你存放 Chromium profile 的目录 |

`gw2a-xkasmvnc.service` 与 `gw2a-vnc-proxy.service` 只在用「VNC 桌面人工登录」时才需要，且里面的 Chromium 路径要按你的系统改。

**5) 安装并启动**

```bash
sudo cp deploy/systemd/*.service deploy/systemd/*.timer /etc/systemd/system/
sudo systemctl daemon-reload

sudo systemctl enable --now gw2a-xkasmvnc            # VNC 桌面（如需人工登录）
sudo systemctl enable --now gw2a-browser-controller  # Chromium 控制器
sudo systemctl enable --now gw2a-cookie-queue        # 队列守护（30min 定时 + 502 触发）
sudo systemctl enable --now gw2a-cookie-watchdog.timer
```

`gw2a-cookie-refresh.timer` 是独立的 30 分钟兜底定时器。队列守护已在做同样的事，**二者留一个即可**，避免重复抓取：

```bash
sudo systemctl disable --now gw2a-cookie-refresh.timer   # 用队列守护时执行这行
```

**6) 首次登录 Google**

```bash
# 打开 Chromium 登录页
/opt/gw2a-cookie-sync/venv/bin/python /opt/gw2a-cookie-sync/gw2a-sync.py open acct1

# 用浏览器访问 VNC 桌面，在桌面上完成 Google 登录
#   http://<你的NAS地址>:16100/
```

**7) 安装扩展**

```bash
python3 /opt/gw2a-cookie-sync/install_ext.py \
        /opt/gw2a-cookie-sync/ext-src \
        --root /opt/gw2a-cookie-sync/ext
sudo systemctl restart gw2a-browser-controller
```

> 首次运行 `install_ext.py` 会生成扩展私钥 `ext/gw2a-ext.pem` 并固定扩展 ID。
> **这个私钥不要提交、不要外发**，丢了扩展 ID 会变，systemd 里的 `GW2A_EXT_ID` 要同步改。

**8) 验证**

```bash
P=/opt/gw2a-cookie-sync/venv/bin/python
S=/opt/gw2a-cookie-sync/gw2a-sync.py

$P $S state acct1      # 期望 logged_in: true, lastSyncOk: true
$P $S sync acct1       # 立刻抓一次 cookie 入池
$P $S pool             # 看池里 browser 来源的账号
curl -s http://127.0.0.1:9280/cookie-pool
journalctl -u gw2a-cookie-queue -f
```

### 部署图片生成插件（可选）

```bash
cd tools/image-gen
docker compose up -d          # 监听 :4010
```

访问 `http://<host>:4010/image-gen.html`。它把非插件请求反代到 `new-api:3000`，所以需要先有名为 `new-api_new-api-network` 的 docker 网络（已在 compose 里声明为 external）。

---

## 关键环境变量

### 控制器 `controller.js`

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GW2A_CTRL_PORT` | `9280` | 控制器监听端口 |
| `GW2A_PROFILE_BASE` | `/vol2/@appdata/gw2a-browser/profiles` | Chromium profile 根目录 |
| `GW2A_CDP_BASE` | `9300` | CDP 调试端口起始值 |
| `GW2A_DISPLAY` | `:13` | X display |
| `GW2A_RUN_USER` | `fygo-browser` | 跑 Chromium 的用户（Chromium 拒绝 root） |
| `GW2A_BROWSER_PROXY` | 空 | 浏览器出口代理，**必须与 cookie 池一致** |
| `GW2A_BROWSER_PROXY_ID` | 空 | 手动指定代理表里的 id，跳过自动匹配 |
| `GW2A_ADMIN_TOKEN` | 空 | gemini-web2api 的面板 token |
| `GW2A_EXT_ID` | 内置 | 扩展 ID |

### Cookie 工具链

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GW2A_PROFILE` | `acct1` | 用哪个 profile |
| `GW2A_API` | `http://127.0.0.1:8083` | gemini-web2api 地址 |
| `GW2A_ADMIN_TOKEN` | 空 | 面板 token |
| `GW2A_COOLDOWN_SEC` | `60` | 刷新后的冷却窗口 |
| `GW2A_MIN_GAP_SEC` | `30` | 两次刷新之间的最小间隔 |
| `GW2A_MAX_REFRESH` | `3` | 一轮最多刷新几次 |
| `GW2A_BUDGET_SEC` | `240` | 一轮总时长上限 |
| `GW2A_SCHED_SEC` | `1800` | 定时周期（秒）= 30 分钟 |
| `GW2A_502_THRESHOLD` | `2` | 累计几个 502 触发重抓 |
| `GW2A_502_COOLDOWN_SEC` | `60` | 502 触发后的防抖冷却 |
| `GW2A_KEEPALIVE_MAX_SEC` | `900` | 保活超过多久补一次（看门狗） |
| `GW2A_SYNC_MAX_SEC` | `2100` | 入池超过多久补一次（看门狗） |
| `GW2A_DB` | 见 `queue_daemon.py` | SQLite 路径 |

---

## 常见问题

**Q：日志里 `logged_in: false`，一直抓不到 cookie。**

按顺序检查：

1. VNC 桌面里的 Chromium 是否真的登录了 Google —— 打开 `gemini.google.com/app` 应该直接进对话页，而不是登录页。
2. 扩展是否装上 —— `curl -s http://127.0.0.1:9280/extension-id` 应该返回扩展 ID。
3. 出口 IP 是否一致 —— 浏览器和 cookie 池必须走同一个出口。NAS 双栈时 v4/v6 出口不一致，直连会被 Google 判定可疑。
4. 扩展 ID 是否与 systemd 里的 `GW2A_EXT_ID` 一致 —— 重装扩展会换 ID。

**Q：所有请求 502。**

先看代理是否通：

```bash
curl -s -o /dev/null -w '%{http_code}\n' -x http://<你的代理> https://gemini.google.com/
```

代理挂了就修代理 —— 这是最常见的 502 原因。修复后队列守护会在 2×502 时自动重抓，无需手动干预。

**Q：浏览器登录缓存被清掉。**

不要用「删除 profile 目录」的方式重置。要重置请用 `setprofile.py` 或在管理面板里操作，避免误删登录态。

**Q：生图请求返回 413。**

图库参考图太大。本项目已在浏览器侧压缩，若仍 413，检查 nginx 的 `client_max_body_size`（`tools/image-gen/nginx.conf` 里是 100m）。

**Q：CPU 占用高。**

早期的实现是每次抓 cookie 都新建 Gemini 会话页，开销大且被风控。本版本改为**刷新已有页面**，CPU 占用显著下降。若仍然偏高，调大 `GW2A_SCHED_SEC`，或在不需要时停掉 `gw2a-cookie-queue`。

---

## 安全提示

- `ADMIN_TOKEN` 不要提交进仓库，用环境变量注入。systemd 单元里的 `CHANGE_ME_ADMIN_TOKEN` 是占位符。
- 扩展私钥 `ext/gw2a-ext.pem` 由 `install_ext.py` 在你自己的机器上生成，**不要提交**。
- Chromium profile 目录含完整 Google 登录态，**不要提交、不要外发**。
- 控制器默认监听 `0.0.0.0:9280` 且无鉴权（可选 `GW2A_SYNC_TOKEN`）。**只在内网暴露**，不要直接映射到公网。
- 本项目会读取你的 Google 会话 cookie 并写入本地 SQLite，请自行评估风险。

---

## 许可证

MIT License —— 详见 [LICENSE](LICENSE)。

上游 `gemini-web2api-go` 同样是 MIT，其原始版权声明已在本仓库 LICENSE 中保留：

```
Copyright (c) 2026 gemini-web2api-go contributors              (上游)
Copyright (c) 2026 gemini-web2api-go-browser-login contributors (本派生版)
```

详细的来源与改动清单见 [NOTICE.md](NOTICE.md)。

再次致谢上游作者 [@zexadev](https://github.com/zexadev)。
