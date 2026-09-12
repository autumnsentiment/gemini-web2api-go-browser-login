# gemini-web2api —— Gemini Web 会话 Cookie 自动供给

## 目标
用服务器自带的 Chromium（VNC 桌面）常驻一个已登录的 Google 会话，通过**扩展**
读取真实会话 cookie，**每 10 分钟保活**、**每 30 分钟重新抓取并写入**
gemini-web2api 的 cookie 池，全程无需人工干预。

## 组件
| 组件 | 位置 | 作用 |
|---|---|---|
| 扩展 `Gemini Cookie Sync` | `/opt/gw2a-cookie-sync/ext` | 在 Chromium 内读 `.google.com` cookie，定时保活/入池 |
| 扩展源码 | `/opt/gw2a-cookie-sync/ext-src` | 改完用 `install_ext.py` 重新打包安装 |
| 扩展私钥 | `/opt/gw2a-cookie-sync/ext/gw2a-ext.pem` | **必须保留**，否则扩展 ID 变化 |
| 控制器 | `/opt/gw2a-browser-controller/controller.js` | 拉起 Chromium(CDP 9300+)，提供 `/cookie-sync` 等接口 |
| 池写入 | `/opt/gw2a-cookie-sync/pool.py` | 直接写容器共享 SQLite（WAL 安全） |
| 命令行 | `/opt/gw2a-cookie-sync/gw2a-sync.py` | state/status/keepalive/sync/pool/open |
| 看门狗 | `gw2a-cookie-watchdog.timer` | 每 5 分钟兜底：补保活、补入池、写状态 |
| VNC 桌面 | `http://NAS_HOST:16100/` | 人工登录 Google 的入口 |

## 扩展 ID
`ommhbjlpchdoohbeamepaddkoidokelc`

## 日常操作
```bash
P=/opt/gw2a-cookie-sync/venv/bin/python
S=/opt/gw2a-cookie-sync/gw2a-sync.py

$P $S state acct1      # 单一 JSON：登录态 + 池 + 扩展配置
$P $S status acct1     # 人类可读
$P $S keepalive acct1  # 立即保活
$P $S sync acct1       # 立即抓 cookie 入池
$P $S pool             # 只看 browser 来源的池账号
$P $S open acct1       # 打开 gemini 登录页（配合 VNC 登录）

cat /var/log/gw2a-cookie-sync-status.json     # 看门狗健康状态
tail -f /var/log/gw2a-cookie-sync.log         # 入池日志
curl -s http://127.0.0.1:9280/cookie-pool     # 控制器视角的池
```

## 登录（只需做一次）
1. 浏览器打开 `http://NAS_HOST:16100/`
2. 桌面上 Chromium 已在 `gemini.google.com/app`，点 **Sign in** 完成 Google 登录
3. 登录成功后扩展会自动入池（最迟 30 分钟内），也可立刻执行 `$P $S sync acct1`
4. 校验：`$P $S state acct1` → `logged_in: true`、`lastSyncOk: true`

会话一旦建立，保活/入池会长期自动维持；若 Google 因长期未用而要求重新验证，
状态文件 `ok` 会变 false 并提示，届时重登一次即可。

## 修改扩展后重新安装
```bash
python3 /opt/gw2a-cookie-sync/install_ext.py \
        /opt/gw2a-cookie-sync/ext-src \
        --root /opt/gw2a-cookie-sync/ext
systemctl restart gw2a-browser-controller   # KillMode=process，不会杀掉已开浏览器
```

## 网络
Chromium 与 cookie 池共用同一出口代理 `http://127.0.0.1:7890`
（`GW2A_BROWSER_PROXY`）。**必须共用**：NAS 是双栈且 v4/v6 出口不一致，
直连会被 Google 判定为可疑并返回 sorry 页；且出口 IP 变化会导致已入池 cookie 失效。

## 出口绑定（重要）
扩展入池时，控制器会把账号 `proxy_id` 自动绑定为浏览器所用出口
（`GW2A_BROWSER_PROXY` 在代理表里对应的 id，当前为 `#2 http://127.0.0.1:7890`）。
原因：gemini-web2api 挑选账号后会用该账号绑定的出口发请求；若账号 `proxy_id=0`
就会走**直连**，与本机 IP 不一致，Google 会立刻把该会话判为可疑并失效。

- 自动匹配：`/cookie-sync` 里 `resolveProxyId()` 按 URL 在 `/admin/api/proxies` 里查 id（结果缓存 5 分钟）
- 手动指定：控制器环境变量 `GW2A_BROWSER_PROXY_ID=2`（跳过自动匹配）
- 代理表里找不到对应 URL 时不会绑定，日志会告警

实测本机出口：代理 `104.244.74.26`，直连 `185.14.47.132` —— 两者不同，必须绑定。

## 会话 cookie 轮换（每 30 分钟）
每 30 分钟的 `gw2a-sync` 闹钟会：
1. 刷新 `gemini.google.com/app`（`keepalive`）→ Google 下发新的
   `__Secure-1PSIDTS` / `SIDCC`
2. 在页面上下文里显式 POST `accounts.google.com/RotateCookies`
   （必须在页面里发：Origin 才是 `gemini.google.com`，实测从 service worker 直发会 400）
3. 重新读取 cookie 并写入 cookie 池（upsert 保号，不新增记录）

每 10 分钟的 `gw2a-keepalive` 只做第 1 步，用于维持会话活跃。
看门狗每 5 分钟兜底：保活超过 15 分钟、入池超过 35 分钟就补一次。
