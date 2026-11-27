# status-monitor

自建 LLM API / HTTP 站点状态页。定时主动探测各渠道，记录可用率与首字延迟（TTFT），
提供状态总览页、历史页和管理后台。单个 Go 二进制 + SQLite，零外部依赖。

## 功能

- **九种探测类型**
  - `openai`：向 OpenAI 兼容接口发流式 chat 请求，测真实 TTFT 和可用性
  - `http`：GET 请求站点，校验状态码（`expect_keyword` 还能匹配正文），测响应耗时
  - `tcp`：连接 host:port；`send`/`expect` 做文本握手，只有 `expect` 时校验服务端
    横幅（SSH/FTP/SMTP 等）；`send` 支持 `\n` `\t` `\xNN` 转义
  - `minecraft`：Server List Ping 协议，返回 MOTD、版本、**在线玩家数**，
    `expect` 可校验 MOTD 文字
  - `steam`：A2S_INFO 查询（CS2 / ARK / 7 Days / 瓦尔海姆等 Source 系），返回**玩家数**
  - `udp`：发自定义 payload（支持 `\xNN` 转义）并匹配回应，适合基岩版 MC
    （`expect: "MCPE"`）等 UDP 游戏服
  - `icmp`：调用系统 `ping` 测网络连通性和 RTT
  - `dns`：解析域名，`expect` 可校验结果 IP 段
  - `mock`：本地模拟，无 key 也能把整套流程跑通

  `minecraft` / `steam` 探测到的玩家数会存库并展示在状态页（"玩家 5/50"），同时按 5 分钟桶计算
  `players_avg`（仅统计有玩家数据的探测），在渠道行下方画出最近 90 格的迷你趋势曲线。
- **状态页**：整体状态横幅、按渠道排序展示 90 格（5 分钟/格）状态条、可用率与平均延迟、状态 favicon；
  游戏服渠道额外显示**玩家数趋势曲线**（数据来自每桶 `players_avg`）
- **历史页**：与参考站一致的动态时间线 —— 按天分组、状态筛选、Markdown 正文、
  更新记录、独立详情页（`#/history/{id}`）
- **公告栏**：发布维护通知/活动公告，置顶显示
- **故障事件**：连续失败自动开单、恢复自动结单（阈值可配）；也可手动发布维护事件
- **维护窗口**：按渠道或全部服务预约维护，自动开始／结束；继续记录探测和真实可用率，暂停自动告警，支持提前结束与历史记录
- **告警**：故障／恢复／响应劣化推送到通用 Webhook、Telegram、钉钉、飞书、企业微信、Discord、Slack、Bark、ntfy；支持同时发送和单平台测试
- **管理后台** `/admin`：渠道增删改/启停、独立探测间隔、立即探测、发布公告、手动故障、测试告警
- **明暗主题**：跟随系统，可手动切换

## 快速开始

```bash
cd app
export ADMIN_TOKEN=dev-token
go run . -config ../config.yaml -addr :8000
```

打开 <http://localhost:8000>，管理后台 <http://localhost:8000/admin>。

默认配置里 `gpt` / `deepseek` 需要设置 `OPENAI_API_KEY` / `DEEPSEEK_API_KEY` 才会探测成功，
未设置时显示「无数据」而不是故障。想先跑通整套流程，可以临时加一个 `type: mock` 的本地模拟渠道，
它在没有外部依赖的情况下产生延迟和随机失败。

## 配置说明

`config.yaml`：

| 字段 | 说明 |
|---|---|
| `site.title` / `site.description` | 页面标题与页脚文案 |
| `server.addr` | 监听地址，默认 `127.0.0.1:18080` |
| `server.tls_cert` / `server.tls_key` | 证书与私钥路径；同时填写即启用 HTTPS |
| `server.http_redirect_addr` | 可选，如 `:80`；HTTP 收到请求后 308 跳到 HTTPS |
| `probe.interval_seconds` | 全局探测间隔，默认 60（渠道可通过 `config.interval_seconds` 覆盖） |
| `probe.timeout_seconds` | 单次探测超时，默认 30 |
| `probe.concurrency` | 并发数，默认 8 |
| `storage.path` | SQLite 路径，默认 `data/status.db` |
| `storage.retention_days` | 探测明细保留天数，默认 30（日汇总固定保留 90 天，供历史页使用） |
| `alerts.enabled` | 是否开启告警；启用时至少配置一个接收端 |
| `alerts.webhook_url` | 通用 JSON webhook 地址，可留空、单独使用 Telegram |
| `alerts.telegram_token` / `alerts.telegram_chat_id` | Telegram 机器人 token 与 chat/group ID，成对填写，支持 `env:VAR` |
| `alerts.dingtalk_webhook_url` / `alerts.dingtalk_secret` | 钉钉自定义机器人，支持可选 HMAC-SHA256 加签 |
| `alerts.feishu_webhook_url` / `alerts.feishu_secret` | 飞书自定义机器人，支持可选签名校验 |
| `alerts.wecom_webhook_url` | 企业微信群机器人 |
| `alerts.discord_webhook_url` / `alerts.slack_webhook_url` | Discord / Slack Incoming Webhook |
| `alerts.bark_device_key` / `alerts.bark_server_url` | Bark 设备 key，可选自建服务器根地址 |
| `alerts.ntfy_url` / `alerts.ntfy_token` | ntfy 完整 topic URL，可选 Bearer token |
| `alerts.message_prefix` | 消息前缀，单行最多 100 字；可匹配机器人的关键词安全设置 |
| `alerts.latency_threshold_ms` | 成功探测响应耗时的告警阈值；0 关闭，范围 1..600000 ms |
| `alerts.fail_threshold` | 连续失败几次判定故障，默认 3 |
| `alerts.recover_threshold` | 连续成功几次判定恢复，默认 1 |
| `channels[].sort` | 正整数升序，同值按 ID；0／省略保留已有顺序，新建默认 1000；首次导入未设值时沿用 YAML 顺序，从 1 开始 |
| `admin.token` | 管理令牌，`env:VAR` 表示读环境变量 |

渠道 `config`、管理员令牌和告警接收端中的 `env:VAR_NAME` 会在运行时读取环境变量。

### Telegram 与延迟告警

在 Telegram 的 @BotFather 创建机器人，先与机器人发起对话，或把机器人加入目标群组并给予发送消息权限。将机器人 token 和目标 chat/group ID 放入环境变量：

```bash
export TELEGRAM_BOT_TOKEN='你的机器人 token'
export TELEGRAM_CHAT_ID='目标聊天或群组 ID'
```

```yaml
alerts:
  enabled: true
  webhook_url: ""  # 可与 Telegram 同时启用，两边独立发送
  telegram_token: "env:TELEGRAM_BOT_TOKEN"
  telegram_chat_id: "env:TELEGRAM_CHAT_ID"
  fail_threshold: 3
  recover_threshold: 1
  latency_threshold_ms: 1000
```

配置变更重启生效。管理后台会显示启用状态及已配置接收端，可选择“全部”或仅测试某个平台。逐项结果显示接收端接受或失败；任一接收端失败，测试接口返回 502，同时保留其他接收端的成功结果。成功只表示平台接受了请求，请在自己的聊天或手机中确认收到。

### 钉钉（含加签）

在钉钉群中添加自定义机器人，复制完整 Webhook 地址。若启用“加签”，还需复制 `SEC...` 开头的密钥。使用关键词安全设置时，将要求的关键词填写为 `message_prefix`；签名校验依赖服务器时钟准确。

```bash
export DINGTALK_WEBHOOK_URL='https://oapi.dingtalk.com/robot/send?access_token=你的token'
export DINGTALK_SECRET='SEC你的密钥'
```

```yaml
alerts:
  enabled: true
  message_prefix: "游戏监控"
  dingtalk_webhook_url: "env:DINGTALK_WEBHOOK_URL"
  dingtalk_secret: "env:DINGTALK_SECRET"
  fail_threshold: 3
  recover_threshold: 1
  latency_threshold_ms: 1000
```

未开启“加签”时，将 `dingtalk_secret` 留空；不要保留指向未设置变量的签名密钥引用，否则启动校验会报错。

### 其他通知平台

把需要的字段合并到同一个 `alerts:` 配置段中，未使用的平台留空即可。下面是字段示例，不需要全部启用：

```yaml
alerts:
  enabled: true
  message_prefix: "游戏监控"
  feishu_webhook_url: "env:FEISHU_WEBHOOK_URL"
  feishu_secret: "env:FEISHU_SECRET"  # 仅开启签名校验时填写
  wecom_webhook_url: "env:WECOM_WEBHOOK_URL"
  discord_webhook_url: "env:DISCORD_WEBHOOK_URL"
  slack_webhook_url: "env:SLACK_WEBHOOK_URL"
  bark_device_key: "env:BARK_DEVICE_KEY"
  bark_server_url: ""  # 默认 https://api.day.app；可换成自己的 Bark 服务器根地址
  ntfy_url: "env:NTFY_URL"  # 完整 topic 地址，如 https://ntfy.example.com/game-status
  ntfy_token: "env:NTFY_TOKEN"  # 无认证时留空
```

- **飞书**：使用群自定义机器人的 Webhook，`feishu_secret` 对应其签名校验密钥。飞书与钉钉的签名规则不同，程序分别处理。
- **企业微信**：填写群机器人地址 `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...`。
- **Discord / Slack**：填写频道的 Incoming Webhook 地址，无须额外 Bot token。正文避免触发意外群体提醒，不发送 `@all`。
- **Bark**：在 iOS Bark 应用中取得设备 key；自建服务器填写根地址，程序调用 `/push`。
- **ntfy**：填写完整 topic URL；开启认证时设置 Bearer token，在 ntfy 客户端订阅同一 topic。

消息按各平台长度限制截断并保留 UTF-8 字符；长消息只发一条，避免分片刷屏。接收端独立并发发送、单次超时 10 秒，失败不自动重发。平台业务错误码会被校验，HTTP 200 不会直接视为成功。结果与日志不回显机器人 URL、token、签名密钥或平台原始错误正文。

Docker Compose 已透传文档中的通知环境变量。可从启动 Compose 的终端导出这些变量，或写入同目录的私有 `.env`；仅在 `config.yaml` 引用需要启用的平台变量即可。

### 通知测试 API

以下接口需要管理员令牌；不会通过公开 `/v1` 暴露通知配置：

```bash
# 仅返回平台名称和是否配置，不返回地址、设备 key 或密钥
curl -H "X-Admin-Token: $TOKEN" localhost:8000/api/admin/alerts

# 只测试钉钉（其余平台不会收到）
curl -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"receiver":"dingtalk"}' localhost:8000/api/admin/alerts/test

# 测试全部已配置平台；旧版空请求体仍兼容
curl -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{}' localhost:8000/api/admin/alerts/test
```

接收端 ID：`webhook`、`telegram`、`dingtalk`、`feishu`、`wecom`、`discord`、`slack`、`bark`、`ntfy`。返回结构包含 `ok` 和 `results[]`（`receiver`、`name`、`ok`、可选 `error`）；部分失败返回 502，全成功返回 200，未配置／无效目标返回 400。

`GET /api/admin/alerts` 还包含 `last_delivery`：最近一次自动告警的 `at`、`event`、`channel`、`ok` 和失败原因。进程重启后清空；手动测试发送不计入。后台的通知区域会直接显示这条记录，告警发送失败时不必再翻日志。

### 延迟通知行为

延迟告警基于成功探测的 `latency_ms`（不使用 TTFT）：首次达到或超过阈值发送 `latency`，连续处于高延迟时不重复发送；降到阈值以下发送 `latency-recovered`。失败探测不会触发延迟恢复，仍走原有故障／恢复通知。延迟告警不改变可用率，也不创建故障事件。

延迟状态目前保存在进程内存中，重启或修改渠道后重新判断；高延迟渠道可能再次收到一次通知。阈值附近的持续抖动仍可能多次跨越阈值，尚未增加防抖时间。

### 定时维护窗口

后台新增独立的“维护窗口”区域，可选择一个或多个渠道，也可明确选择“所有服务”。开始时间留空表示立即开始；结束时间必填，最长 30 天，可预约未来 365 天内的时间。浏览器按本地时区输入，接口保存 Unix 秒。维护窗口存在 SQLite，进程重启后仍有效，不依赖浏览器保持打开。

维护生效期间：

- 探测继续运行，失败结果、响应耗时和历史状态条照常保存，**可用率仍按真实成功率计算**。
- 所选渠道不发故障、恢复、延迟或延迟恢复通知，也不新建／关闭自动故障事件。管理员主动点击“发送测试告警”仍可测试通知链路。
- 状态页用蓝色维护标签和横幅说明原因与起止时间；其他未维护服务的故障继续正常显示、告警。
- 普通动态表单中的“维护公告”仅发布信息，**不暂停告警**；只有维护窗口执行上述行为。

到期或提前结束后，下一个有效探测按正常规则重新判断。维护期间的失败不计入后续连续失败次数；已有自动故障事件在恢复探测后按恢复阈值关闭，手动故障事件不受影响。窗口重叠时，须等该渠道的所有窗口均结束后才恢复告警。跨越维护边界的在途探测也会被标记为维护探测。

计划可以取消，进行中的维护可以提前结束；已结束记录保留在后台与历史动态中，详情 ID 为 `m123`。所有服务范围自动包括之后新建的渠道；指定渠道范围固定。维护创建／结束本身不会自动群发通知；已发出的消息也无法撤回。

维护读取接口：

- `GET /api/maintenance`、`GET /v1/maintenance`：尚未结束且未取消的窗口，最多 200 条。
- `/api/status`、`/v1/status` 增加顶层 `maintenance[]`；每渠道增加 `maintenance`（不在维护中时为 null）。`current_status` 继续使用原枚举，维护中为 `partial`；兼容输出映射为黄色。
- `/v1` 展示仍受 `api.cache_seconds` 缓存影响；告警抑制每次直接读取数据库，不依赖展示缓存。

管理接口（需管理员 token）：

```bash
# 立即维护指定渠道一小时；结束时间使用 Unix 秒
END=$(($(date +%s) + 3600))
curl -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"title\":\"游戏服更新\",\"start_at\":0,\"end_at\":$END,\"channel_ids\":[\"game-mc\"]}" \
  localhost:8000/api/admin/maintenance

# 查看所有维护记录（包括已结束／已取消）
curl -H "X-Admin-Token: $TOKEN" localhost:8000/api/admin/maintenance

# 取消计划／提前结束 id=1 的维护；不会删除历史记录
curl -X POST -H "X-Admin-Token: $TOKEN" localhost:8000/api/admin/maintenance/1/cancel
```

创建返回 201 和窗口对象。全部服务使用 `"all_channels":true` 并省略 `channel_ids`。范围／时间错误返回 400，未授权返回 401，已结束／不存在的取消目标返回 404。创建和取消无需重启服务。

### 渠道排序与状态图标

后台编辑渠道可设置 `sort`，范围为 0..2147483647 的整数。正整数越小越靠前；0／省略表示保留该渠道原顺序，新建渠道默认为 1000。首次导入 YAML 时显式 `sort` 优先，未填写则按文件顺序从 1 开始。旧库中为 0 的排序会迁为 1000。所有已有探测记录和事件均保留。

首页按接口返回的顺序显示渠道，不再按分组重新排列；分组名称仍显示在渠道行。标签页 favicon 随当前整体状态显示绿／黄／红／灰色，状态加载失败时变灰并提示数据可能过期。

### 渠道配置示例

```yaml
channels:
  # OpenAI 兼容（OpenAI / DeepSeek / Kimi / 中转站等）
  - id: my-gpt
    name: "GPT"
    group: "GPT"
    type: openai
    config:
      base_url: "https://api.example.com/v1"
      api_key: "env:MY_API_KEY"
      model: "gpt-4o-mini"
      stream: true
      max_tokens: 16
      interval_seconds: 60    # 可选：覆盖全局探测间隔（>=10）

  # Minecraft：协议级探测，返回 MOTD/版本/在线玩家数
  - id: game-mc
    name: "Minecraft"
    group: "游戏"
    type: minecraft
    config:
      host: "127.0.0.1"
      port: 25565
      interval_seconds: 60
      # expect: "生存服"     # 可选：校验 MOTD

  # Steam/Source 引擎（CS2/ARK/7 Days/瓦尔海姆等）：返回玩家数
  - id: game-cs
    name: "CS2"
    group: "游戏"
    type: steam
    config:
      host: "127.0.0.1"
      port: 27015

  # 通用 UDP（基岩版等）：发包 + 关键字匹配
  - id: game-bedrock
    name: "基岩版"
    group: "游戏"
    type: udp
    config:
      host: "127.0.0.1"
      port: 19132
      send: "\x62\x01\x05\x00"
      expect: "MCPE"

  # SSH：服务端先发横幅，只写 expect 即可
  - id: bastion
    name: "跳板机"
    group: "网络"
    type: tcp
    config:
      host: "127.0.0.1"
      port: 22
      expect: "SSH-2.0"

  # 普通站点
  - id: site
    name: "官网"
    group: "站点"
    type: http
    config:
      url: "https://example.com"
      expect_status: [200, 204]     # 支持整数或数组
      expect_keyword: "Example"     # 可选：正文需包含该字符串
```

注意：`config.yaml` 里的渠道只在**首次初始化**时导入数据库（之后以后台数据为准）。
后台删除的渠道不会在重启后复活。想重新导入，需要清空 `data/status.db`。

## API

公开：

- `GET /api/status?window=27000` — 状态快照
- `GET /api/history?days=30` — 历史聚合
- `GET /api/dynamics` — 动态列表（公告/维护/故障，含更新记录）
- `GET /api/dynamics/{id}` — 单条动态，`id` 形如 `d1`（人工）或 `i3`（探测自动生成）

管理（`X-Admin-Token` 头或 `Authorization: Bearer`）：

```bash
TOKEN=dev-token
# 渠道
curl -H "X-Admin-Token: $TOKEN" localhost:8000/api/admin/channels
curl -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"new","name":"新渠道","group":"默认","type":"mock","config":{}}' \
  localhost:8000/api/admin/channels
curl -X DELETE -H "X-Admin-Token: $TOKEN" localhost:8000/api/admin/channels/new
curl -X POST -H "X-Admin-Token: $TOKEN" localhost:8000/api/admin/probe/new

# 动态（公告 / 维护 / 故障），正文支持简单 Markdown
curl -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"maintenance","state":"active","title":"世界服重启维护",
       "body":"## 维护窗口\n\n- 22:00-23:00","services":["世界服"],"pinned":true}' \
  localhost:8000/api/admin/dynamics
curl -H "X-Admin-Token: $TOKEN" localhost:8000/api/admin/dynamics
curl -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"label":"已定位","body":"连接池耗尽"}' localhost:8000/api/admin/dynamics/d1/updates
curl -X POST -H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"state":"resolved"}' localhost:8000/api/admin/dynamics/d1
curl -X DELETE -H "X-Admin-Token: $TOKEN" localhost:8000/api/admin/dynamics/d1

# 测试告警（需要 alerts.enabled=true 且至少配置 webhook 或 telegram 之一）
curl -X POST -H "X-Admin-Token: $TOKEN" localhost:8000/api/admin/alerts/test
```

公开接口还有 `GET /api/incidents`（事件列表）和 `GET /api/health`（健康检查）。健康检查会返回 `last_probe_age_seconds` 和 `stale_after_seconds`；当调度器超过 `2×探测间隔+超时`（最少 60 秒）没有写入任何探测时返回 503，可用来发现「进程还活着但探测已停摆」的情况。启动后的第一个周期内不判定为异常，没有启用渠道时也不判定。

### 对外公开 API（/v1/*）

供第三方、玩家社区嵌页面使用的稳定契约，在 `config.yaml` 打开：

```yaml
api:
  enabled: true
  cors_origins: ["*"]        # 或精确来源 ["https://guild.example.com"]
  rate_limit_rpm: 120        # 每 IP 每分钟
  cache_seconds: 10          # 服务端响应缓存
```

| 端点 | 说明 |
|---|---|
| `GET /v1/status?window=27000` | 状态快照（加 `&format=lcapi` 输出参考站形状） |
| `GET /v1/history?days=30` | 历史聚合 |
| `GET /v1/dynamics` | 动态列表 |
| `GET /v1/dynamics/{id}` | 单条动态 |
| `GET /v1/maintenance` | 当前与即将开始的维护窗口 |
| `GET /v1/health` | 存活检查 |

特性：

- **CORS**：按 `cors_origins` 白名单放行跨域；预检请求返回 204。默认 `*`。
- **限流**：每 IP 令牌桶，`rate_limit_rpm` 既是速率也是突发容量；超限返回
  `429` + `Retry-After`，响应带 `X-RateLimit-Remaining`。
- **缓存**：200 响应在内存缓存 `cache_seconds` 秒，重复轮询直接命中
  （`X-Cache: HIT`），并带 `Cache-Control: public, max-age=N` 供 CDN 复用。
- **稳定契约**：`/v1/*` 字段结构只加不改；内部 `/api/*` 与 `/v1/*` 互不影响，
  管理接口不在公开面（`/v1/admin/*` 返回 404）。

给玩家社区嵌入示例：

```html
<script>
fetch("https://status.example.com/v1/status")
  .then(r => r.json())
  .then(d => console.log(d.channels.map(c => `${c.name}: ${c.current_status}`)));
</script>
```

### lcapi 兼容格式

任何前端或脚本如果按 status.lcapi.online 的字段形状写的，加 `?format=lcapi`
即可直接对接，**默认格式不受影响**：

```bash
curl "localhost:8000/api/status?format=lcapi"
curl "localhost:8000/api/dynamics?format=lcapi"
```

映射规则：

| 本项目 | lcapi 兼容输出 |
|---|---|
| `channels[]` | `groups[]`（`id`/`name`/`category`(=group)/`multiplier`(=1)/`status`(green/yellow/red/unknown)/`uptime`） |
| `buckets[]` | `points[]`（`at`/`duration_seconds`/`status`(green/yellow/red/unknown)/`total`/`failures`/`ttft_ms`/`source`/`probe_count`/`probe_failures`） |
| `overall`（自算） | `green`/`yellow`/`red` + `summary` 文案 |
| `window_seconds`/`bucket_seconds` | `window_minutes`/`bucket_minutes`/`minutes` |
| `site.description` | `site.footer` |
| 动态 `id: "d1"` / `"i3"` / `"m4"` | 数字 `id`（自动事件加 1_000_000、维护窗口加 2_000_000 偏移），原字符串 id 保留在 `ref` |

## 部署

### Docker

```bash
docker compose up -d --build
```

数据落在 `./data`，配置挂载 `./config.yaml`。

### 裸机

```bash
cd /你的路径/status-monitor
export ADMIN_TOKEN='你的管理员令牌'
./run.sh
```

脚本从项目根目录启动，默认配置为 `config.yaml`，相对数据库路径从项目根目录解析。设置 `STATUS_CONFIG` 可使用另一份配置；`ADDR` 仅在显式设置时覆盖 `server.addr`。项目根目录存在 `.env` 时会自动加载（适合放 `ADMIN_TOKEN`，已加入 `.gitignore`）。有 Go 工具链时，仅当 `app/` 内有文件比二进制新才重新编译，并原子替换二进制；无 Go 时使用已编译的 `status-monitor`。

需要单独构建：

```bash
(cd app && go build -o ../status-monitor .)
ADMIN_TOKEN=xxx ./status-monitor -config config.yaml
```

默认监听 `127.0.0.1:18080`（可用 `-addr` 覆盖）；静态页面已用 `go:embed` 打进二进制，
部署时只需要二进制 + `config.yaml`。

### 反向代理示例

Caddy（自动 HTTPS）：

```caddy
status.example.com {
    reverse_proxy 127.0.0.1:18080
}
```

nginx：

```nginx
server {
    listen 443 ssl;
    server_name status.example.com;
    ssl_certificate     /etc/letsencrypt/live/status.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/status.example.com/privkey.pem;
    location / {
        proxy_pass http://127.0.0.1:18080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
```

注意：走反代时不要在 config.yaml 里配 `tls_cert`（TLS 由反代终止）；
`api.rate_limit_rpm` 的限流按连接 IP 计，需要按真实客户端 IP 限流时再加 `trusted_proxy` 支持。

### systemd 示例：

```ini
[Unit]
Description=Service status monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/status-monitor
EnvironmentFile=/opt/status-monitor/.env
Environment=STATUS_CONFIG=/opt/status-monitor/config.local.yaml
Environment=HOME=/root
ExecStart=/opt/status-monitor/run.sh
Restart=always
RestartSec=5
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
```

`HOME` 必须设置，否则有 Go 工具链时编译会因找不到模块缓存失败。`run.sh` 会在启动时加载 `.env`，所以 `ADMIN_TOKEN` 放在里面即可，不必写进 unit。

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now status-monitor
sudo systemctl status status-monitor
```

### 数据库备份

```bash
./backup.sh                      # 备份 data/status.db 到 data/backups/
BACKUP_DIR=/mnt/backup ./backup.sh
./backup.sh /path/to/other.db    # 指定其他数据库
```

使用 SQLite 在线备份 API，服务运行中也能得到一致快照；备份完成后对副本做 `PRAGMA integrity_check`，文件权限为 600，目录为 700。脚本不会自动删除旧备份。

### 探测项目自身时的注意事项

服务账号需要对项目 `data/` 有读写权限；`ADMIN_TOKEN` 及可选 Telegram 环境变量写入项目根目录的 `.env`。同一个数据库只启动一个监控实例，升级时用服务管理器重启。

- GPT/DeepSeek 等渠道探测失败但接口返回 401/402 时，会记成"故障"——这是预期行为，
  能反映 key 失效或余额不足。
- 默认 `window=27000`（7.5 小时），和参考站一致；调大需要更多格子，前端自适应。
- 想在公网暴露管理接口，请务必设置强随机 `ADMIN_TOKEN`，并按需在反向代理层限制 `/admin`。
- 渠道 `config.api_key` 支持 `env:VAR`；若变量未设置，该渠道会记录为配置错误且不发起外部请求。配置错误（缺少 url、key、host、model 或未知渠道类型）不会计入可用率、不会显示为故障、也不触发告警，状态页显示「无数据」；修好配置后按正常探测恢复。
- HTTPS 建议用证书文件（Let's Encrypt 的 `fullchain.pem` + `privkey.pem`），
  反代终止 TLS 也行；此时不要把 `tls_cert` 填成反代证书。
- `minecraft` 探测需要在服务器防火墙放行查询端口（和游戏端口相同）；`steam` 走 UDP 查询端口。
- `icmp` 依赖系统 `ping` 命令；容器里跑需要镜像内有 `iputils-ping`（Alpine 镜像已包含 busybox ping）。
- `interval_seconds` 单渠道值会同时影响下次探测时间与"数据过期"判定（超过 `2×interval+timeout` 视为无数据）。

## 测试

```bash
cd app
go test ./...
go test -race ./...
```

通知管理界面的脚本回归（需 Node.js，无额外依赖）：

```bash
node tests/notifications-ui.test.cjs
node --test tests/maintenance-ui.test.cjs
```

编译后二进制的本地通知验收（需 Python 3 标准库，临时数据库和接收端会自动清理）：

```bash
(cd app && go build -o ../status-monitor .)
python3 tests/notifications-live.py ./status-monitor
python3 tests/maintenance-live.py ./status-monitor
```

新增回归覆盖各通知平台成功／失败响应、钉钉／飞书签名固定向量、UTF-8 消息截断、并行发送与部分失败、指定接收端测试及鉴权／脱敏。所有通知测试使用本地 HTTP Transport 或本机测试服务器，不访问真实平台，也不向群聊发送消息。另覆盖延迟跨阈值通知、排序保存／回读／重启保留。

覆盖：快照桶边界（90 格含当前桶）、过期判定、历史日汇总在明细清理后仍可查、
手动事件不被自动恢复关单、渠道只在首次初始化导入、HTTP 状态码/关键词校验、OpenAI 流式首字。
