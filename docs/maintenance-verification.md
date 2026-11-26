# 定时维护窗口：本轮交付核验

## 本轮目标

用户授权自主迭代并保留现有功能。本轮选定的可验收增量是：为游戏服及其他服务新增定时维护窗口，支持指定渠道／全部渠道、预约开始、自动结束、提前取消；维护期间继续探测并保留真实统计，但暂停自动告警；后台可操作，公开页面与动态流可查看；更新现有服务且不改变既有真实监控配置。

## 交付与证据对应

| 交付项 | 实现文件 | 实际验证 |
|---|---|---|
| SQLite 窗口、范围、起止时间、取消状态与升级迁移 | `app/maintenance.go`、`app/store.go` | `TestMaintenanceLifecycleAndScope`、`TestMaintenanceCancellationAndOverlap`、`TestMaintenanceInputValidation`、`TestMaintenanceMigrationPreservesLegacyData` |
| 创建／查看／取消管理 API，公开只读信息与鉴权隔离 | `app/maintenance_api.go`、`app/main.go`、`app/api_public.go` | `TestMaintenanceAPIAuthLifecycleAndPublicIsolation`、`TestMaintenanceAPIValidation`；运行实例 `/api/maintenance`、`/v1/maintenance` 返回 200，未授权管理接口返回 401 |
| 维护继续探测、不伪造成功率和历史桶 | `app/store.go`、`app/scheduler.go` | `TestMaintenanceStatusKeepsProbeEvidence`、`TestMaintenanceSuppressesFailureAndLatencyWhileRecording`；独立服务定时产生失败探测，记录存在、availability=0、失败桶仍为 down，无告警／自动故障事件 |
| 故障／恢复／延迟通知暂停，到期／取消后重计阈值 | `app/scheduler.go` | `TestMaintenanceSuppressesFailureAndLatencyWhileRecording`、`TestMaintenanceCancelRestoresLatencyNotifications`；本地独立服务结束维护后第一次失败不告警，第二次失败发 down，后续成功发 recovered |
| 重启与边界正确，重叠窗口持续覆盖、在途探测正确标记 | `app/maintenance.go`、`app/scheduler.go` | `TestMaintenanceRestartRestoresOnlyPostWindowStreak`、`TestMaintenanceCreatedWhileProbeInFlight`、`TestMaintenanceCancellationAndOverlap`；独立服务重启后维护仍静默 |
| 不误关闭已有手动事件 | `app/scheduler.go` | `TestMaintenanceDoesNotClosePreexistingIncidents` |
| 后台独立维护表单、范围选择、本地时间输入、取消确认、错误和忙状态 | `app/static/admin.html` | `tests/maintenance-ui.test.cjs` 执行实际页面脚本，12/12 子测试通过；时间转换、重复提交、错误反馈、退出清理与 XSS 均覆盖 |
| 蓝色维护标签、预告／进行中横幅、其他服务故障优先；历史筛选和详情 | `app/static/index.html`、`app/maintenance_dynamics.go`、`app/dynamics.go` | `TestMaintenanceDynamicsAndCancellationHistory`；同一 UI 脚本测试覆盖维护／真实故障并存、原始桶、pending 和维护筛选、`m123` 路由 |
| 原功能保留 | 既有 probe、alerts、compat、public API 与页面 | 全套 Go 测试含 race 通过；原 `notifications-ui.test.cjs` 通过；`notifications-live.py` 对本地八个接收端回归通过；运行实例原状态／历史／动态／v1／lcapi API 均返回 200 |
| 配置及操作说明 | `README.md` | 已列范围、时间、告警语义、只读字段、管理请求、维护公告与窗口区别、测试命令与当前缓存限制 |
| 当前服务加载新版本、数据保留、单实例 | 项目 `status-monitor` 二进制、既有 `config.local.yaml` | HTTPS 验证证书后请求正常；`/` 与 `/admin` 内容逐字节等于本地新 HTML；仅一个 monitor 进程；备份与升级后行数比对见下 |

## 本轮执行过的检查

- `go vet ./...`：通过。
- `go test -race ./... -count=1 -timeout=90s`：通过，共 84 个顶层 Test 函数（另含表驱动子测试）。
- `node --test tests/maintenance-ui.test.cjs`：12/12 通过。
- `node tests/notifications-ui.test.cjs`：通过。
- `python3 tests/maintenance-live.py ./status-monitor.next`：通过。使用临时数据库和本地接收端，验收真实定时探测、进程重启、提前结束、未来开始、自然到期和告警恢复，自动清理测试实例。
- `python3 tests/notifications-live.py ./status-monitor.next`：通过，未向外部平台发送消息。
- 运行实例 `https://127.0.0.1:18443`：`/api/status`、`/api/history?days=90`、`/api/dynamics`、`/api/maintenance`、`/v1/status`、`/v1/status?format=lcapi`、`/v1/maintenance` 均 200。
- 运行实例管理维护列表为 `[]`，公开状态的维护列表为空、每渠道 maintenance 为 null：升级没有给现有服务自动安排维护。

## 运行数据核对

升级前一致性备份：`data/pre-maintenance-update-20261116-214146.sqlite`。

| 数据 | 备份行数 | 升级后核验行数 |
|---|---:|---:|
| channels | 12 | 12 |
| probes | 34120 | 34174 |
| incidents | 99 | 99 |
| dynamics | 1 | 1 |

新 `maintenance_windows`／`maintenance_channels` 表与 `probes.maintenance_id` 字段已迁入；原始数据保留，探测继续写入。

## 验证边界与使用限制

- 当前会话没有浏览器插件要求的 Node REPL 浏览器控制工具，因此**未进行本轮浏览器视觉截图验收**。UI 证据为实际内联脚本的内存 DOM 交互测试、输出转义检查，以及运行服务返回的 HTML 与源码一致性检查；不将这些声称为真实浏览器视觉检查。
- 本轮没有替用户创建真实维护窗口，也未发送真实第三方通知；维护通知语义在临时服务与本地接收端验证。
- 维护窗口只控制自动探测告警，管理员测试通知不受影响；已发出的通知无法撤回。
- 公开 `/v1` 显示受已有短 TTL 缓存影响，告警抑制直接读取存储。普通维护公告不暂停告警，只有维护窗口生效。
- 本记录描述本轮增量交付，不将开放式长期迭代目标声明为结束。
