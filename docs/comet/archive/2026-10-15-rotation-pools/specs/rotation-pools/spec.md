# 轮转池（Rotation Pools）

## 概述

轮转池是控制台设置中的新标签页，允许多个池共存。每个池绑定一个 Cloudflare 域名与专属 DDNS 凭据，并关联多个已配置的云端实例。每天在全局轮转时刻，池内按顺序激活下一台实例：开机 → 轮询获取公网 IP → 绑定域名 → 关机其余实例。调度复用现有 Engine 调度循环与 Job 体系。

## 数据模型

`RotationPool`：

- `id`：稳定主键（SQLite `rotation_pools` 表）。
- `name`：池名称，必填，用于展示。
- `cf_token`：Cloudflare API Token（AES-GCM 加密存储；回显脱敏，留空保持不变）。
- `cf_zone_id`：Zone ID，可留空，留空时按域名自动解析。
- `domain`：池域名（A 记录名），必填，如 `relay.example.com`。
- `enabled`：池启用开关。
- 全局设置新增「轮转时刻」（`rotation_time`，`HH:MM`，默认 `00:00`，存 settings 表）：所有池共用同一轮转时刻，池级不单独配置时刻。
- `account_ids`：池内实例（`accounts` 表已有实例）的有序列表；同一实例只能属于一个池。
- 轮转运行状态（持久化，独立于配置）：`current_account_id`（当前活跃实例）、`last_rotate_date`（`YYYY-MM-DD`，业务时区）。

## 配置与 API

- 轮转池随 `GET/PUT /api/v1/config` 一起读写（`rotation_pools` 字段），保存走现有事务与加密体系。
- 保存校验：
  - 池内实例必须存在且未软删除；同一实例不得出现在多个池。
  - 互斥约束（见下）。
  - `domain` 非空、格式合法；全局 `rotation_time` 为 24 小时制 `HH:MM`。
  - 池内至少 1 个实例才可启用池（启用时校验）。
- 实例已在轮转池中时，开启 `schedule_enabled` → 保存被拒绝，提示互斥。
- 双向校验发生在 `PUT /api/v1/config`（同一事务内，配置保存与池保存原子生效）。
- 运行时兜底：轮转调度跳过 `schedule_enabled=true` 的池内实例并记录告警日志；每日定时调度跳过池内实例。
- 保活互斥：`processAccount` 的抢占式保活分支跳过所有池内实例（无论实例级或全局 `keep_alive` 开启），防止轮转关机后被保活逻辑重新开机。

## 轮转调度（按天）

- 检测点：现有 Engine 15 秒 scheduler 循环。每轮读取启用且池内 ≥2 实例的池，判断当前业务时区日期与时刻是否到达全局 `rotation_time`（含重启补偿窗口，与现有 `dueWithin` 一致的短窗口语义）。
- 到达时刻后（每池每天只执行一次，以 `last_rotate_date` + 动作事件键去重）：
  1. 计算下一台实例：池内 `account_ids` 顺序，按 `last_rotate_date` 与当日的天数差推进位置；重启后按日期差补进，不重放历史切换。
  2. 通过现有 Job/Worker 体系执行：先开机新实例（复用现有 ControlInstance 通道与 `accountLock`）。
  3. 开机指令成功后轮询获取该实例公网 IPv4：每 10 秒调用一次 `DescribeInstances`（`PublicIpAddress.IpAddress[0]` 优先，为空取 `EipAddress.IpAddress`），最多 12 次（约 2 分钟）；仅接受公网 IPv4（拒绝私网/保留地址）；超时视为切换失败。
  4. 更新/创建池域名的 Cloudflare A 记录（`type=A, name=domain, content=ip, ttl=1, proxied, comment="Managed by CDT Monitor"`）；Zone ID 留空时先按域名解析 Zone。记录存在则 PUT、不存在则 POST（先 GET 查询）。
  5. DDNS 成功后才关机其余池内实例（关机模式沿用全局/实例 `shutdown_mode` 解析）。
  6. 更新 `current_account_id` 与 `last_rotate_date`。
- 切换失败（开机失败、IP 轮询超时、DDNS 更新失败）：不关机旧实例，写错误日志，并通过现有通知渠道（`NotificationEvent` + Outbox）发送告警；本日不重试切换。切换成功只写运行日志，不发送通知。
- 池内实例被删除/软删时从轮转序列剔除；池内仅剩 1 实例时不再切换（保持该实例运行）。

## 手动操作

- **立即轮转**：`POST /api/v1/rotation-pools/{id}/rotate`（admin 权限）。入队一个 `rotate_pool` Job（uniqueKey 含时间戳，允许重复手动触发），复用 `runRotation` 全流程与失败保护；池不满足轮转条件（未启用/实例 <2）时返回 400 与原因。前端在池编辑器提供「立即轮转」按钮，显示 Job 提交结果（202 + job_id），执行结果可在日志查看。
- **测试 DDNS**：`POST /api/v1/rotation-pools/{id}/test-ddns`（admin 权限）。使用池已存 Token 调用 Cloudflare 只读 API：Token 有效性（zones 查询）、Zone 解析（显式 Zone ID 或按域名解析）、A 记录查询（记录存在时返回当前指向的 IP）。全程 GET，不创建/修改/删除任何记录。返回结构化结果 `{success, message, details}`；Token 未配置或池不存在返回 400。前端在池编辑器提供「测试」按钮并显示结果。

## 前端（控制台设置 → 轮转池标签页）

- 新标签页「轮转池」（图标 + 文案风格与现有标签一致），布局类似「实例」标签页：池列表 + 「添加轮转池」按钮，每池一个可折叠编辑器（名称、Token、Zone ID、域名、Proxied、启用、实例关联多选）；全局「轮转时刻」配置放在「系统」标签页的监控策略区（与系统时区相邻）。
- 实例关联选择器只列出未开启「每日定时开关机」的实例；选中即关联，保存时后端仍做最终校验。
- 池状态网格：每池下方紧密网格展示池内实例，每项 = 实例备注（取实例配置 `remark`，为空回退实例 ID）+ 状态圆点：绿=Running、红=Stopped、灰=Unknown/Starting/Stopping。当前活跃实例高亮标识。
- 样式复用现有 `glass-panel`/`form-grid`/`toggle-list` 等既有类与设计语言，不引入新 UI 框架。

## 验收场景

Scenario: 设置中创建与编辑多个轮转池
管理员打开控制台设置，切换到「轮转池」标签页，可添加多个池并分别配置名称、Cloudflare Token、Zone ID、域名、Proxied 与实例关联；「系统」标签页配置全局轮转时刻；保存后重新打开设置，配置完整回显（Token 脱敏）。删除池不影响实例本身。
验收：A1

Scenario: Token 加密存储
保存含 Cloudflare Token 的池配置后，SQLite 中存储的是 AES-GCM 密文；GET config 回显 `token_configured=true` 且不泄露明文；留空保存不改变已存 Token。
验收：A2

Scenario: 互斥约束（先定时后入池）
实例 A 已开启「每日定时开关机」，将其加入轮转池并保存 → 请求被拒绝，错误信息说明互斥；实例与池配置均不变。
验收：A3

Scenario: 互斥约束（先入池后定时）
实例 B 已在轮转池中，在「实例」标签页为其开启「每日定时开关机」并保存 → 请求被拒绝，错误信息说明互斥。
验收：A3

Scenario: 每日轮转与域名绑定
启用池（≥2 实例）到达全局轮转时刻：新实例开机成功 → 轮询获取公网 IPv4 → 池域名 A 记录更新为新实例公网 IPv4 → 其余池内实例关机；`current_account_id`/`last_rotate_date` 更新。次日同一时刻轮转到下一台。进程重启后按日期差推进，不重复切换。
验收：A4

Scenario: 切换失败保护与告警
开机失败、IP 轮询超时或 Cloudflare 更新失败（网络/凭据错误）→ 旧实例保持运行（不关机），错误写入日志并通过现有通知渠道告警，本日不再重试切换。
验收：A4

Scenario: 池状态网格
轮转池标签页中每个池展示实例网格：备注 + 圆点（绿=Running、红=Stopped、灰=Unknown/Starting/Stopping），布局紧凑并与现有设置面板风格一致。
验收：A5

Scenario: 保活互斥
池内实例被轮转关机后，即使全局或实例级「抢占式实例保活」开启，监控周期也不会将其重新开机（保活分支跳过池内实例）；非池内实例的保活行为不变。
验收：A6

Scenario: 立即轮转
管理员在轮转池标签页点击某池的「立即轮转」按钮：请求返回 202 + job_id；Job 执行与定时轮转相同的流程（开机下一台 → IP 轮询 → DDNS 绑定 → 关机其余），失败保护与告警一致；池未启用或实例不足时返回 400 与原因。
验收：A7

Scenario: DDNS 配置测试
管理员在轮转池标签页点击某池的「测试」按钮：后端用已存 Token 调用 Cloudflare zones/records 只读 API；配置正确时返回成功与当前 A 记录指向（如有）；Token 无效/Zone 不存在/网络错误时返回具体错误信息。不修改任何 DNS 记录。
验收：A8
