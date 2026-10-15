# 目标

在 CDT Monitor 控制台设置中新增「轮转池」标签页，支持多个轮转池共存。每个轮转池配置专属的 Cloudflare DDNS 凭据（API Token、Zone ID、域名、Proxied）并关联多个已有云端实例；每天按池内实例顺序轮转开机一台实例、关机其余实例，并在成功开机后把池域名解析绑定到当前活跃实例的公网 IPv4。轮转池调度复用现有 Engine 15 秒调度循环与监控 Job 体系，不引入新的定时任务框架或设置。轮转池实例与「每日定时开关机」互斥：无论先后，禁止另一方操作。轮转池内实例状态以紧密网格展示，每项为「备注 + 绿/红/灰圆点」。

# 范围

## Source coverage

需求来源为用户消息（含补充指令）与 `ecs-controller/` 参考实现。用户指定的覆盖边界：DDNS 配置方式与轮转调度行为参考 ecs-controller；其余以用户消息为准。

| 来源条目与位置 | 读取状态 | 需要保留的内容 | Spec 位置 | 验收 ID | 覆盖状态 | 理由或替代关系 |
| --- | --- | --- | --- | --- | --- | --- |
| S1：用户消息功能 1（轮转池设置） | complete | 控制台设置新增轮转池标签页，布局类似「实例」标签页，多池共存；每池专属 Cloudflare DDNS 密钥/域名等配置；池关联多个实例，每次只激活一个 | specs/rotation-pools/spec.md | A1, A2 | covered | 当前有效需求 |
| S2：用户消息功能 1（互斥约束） | complete | 轮转池实例禁止开启「每日定时开关机」；无论先加入池还是先开启定时，都禁止另一方操作 | specs/rotation-pools/spec.md | A3 | covered | 当前有效需求 |
| S3：用户消息功能 2（按天轮转） | complete | 池按天对池内实例轮转开机，并对旧实例关机 | specs/rotation-pools/spec.md | A4 | covered | 当前有效需求 |
| S4：用户消息功能 2（网格状态） | complete | 池内实例状态用紧密网格布局组合，每项为备注 + 绿/红/灰圆点，备注取自实例配置 | specs/rotation-pools/spec.md | A5 | covered | 当前有效需求 |
| S5：用户消息功能 3（样式与分支） | complete | 整体样式协调；新分支开发 | — | — | covered | 分支已创建 comet/rotation-pools；样式为验收观察项并入 A5 |
| S6：用户补充指令（域名绑定） | complete | 成功开机新实例后，将当前轮转池配置的域名绑定到该实例（DDNS A 记录指向其公网 IP） | specs/rotation-pools/spec.md | A4 | covered | 当前有效需求 |
| S7：用户补充指令（调度复用） | complete | 轮转池按当前定时任务的设置进行定时调度，不另外引入新的定时任务框架或设置 | specs/rotation-pools/spec.md | A4 | covered | 复用 Engine scheduler 15s 循环与 Job/Worker 体系 |
| S8：ecs-controller DdnsService.php（Cloudflare API 用法） | complete | Bearer Token 认证；zones 查询或显式 zone_id；A 记录 upsert（type/name/content/ttl=1/proxied/comment）；公网 IPv4 校验；错误格式化 | specs/rotation-pools/spec.md | A4 | covered | 实现参考，行为并入 Spec |
| S9：ecs-controller DdnsScheduler.php（轮转调度参考） | complete | day_cycle 锚点 + 顺序轮转 + duration 权重；切换时先开机新实例、成功后关机其余；DDNS 失败则中止切换 | specs/rotation-pools/spec.md | A4 | covered | 实现参考；Go 版按天轮转简化 |
| S10：ecs-controller AliyunService.php:619-653（公网 IP 获取） | complete | DescribeInstances 返回 PublicIpAddress.IpAddress[0] 优先、EipAddress.IpAddress 兜底 | specs/rotation-pools/spec.md | A4 | covered | 实现参考 |

## 实现范围

- 后端：`internal/domain`（RotationPool 模型）、`internal/store`（rotation_pools 表 + 迁移 + 配置读写 + Token 加密）、`internal/aliyun`（DescribeInstances 公网 IP 查询）、`internal/notify`（Cloudflare 客户端）、`internal/engine`（轮转调度逻辑，挂在现有 scheduler/Job 体系）、`internal/httpapi`（配置校验与互斥拒绝）。
- 前端：`web/src`（设置面板新增「轮转池」标签页、池编辑器、网格状态展示）。
- 文档：README 功能列表补充轮转池。

# 非目标

- 不引入新的定时任务框架、cron 表达式或额外调度间隔设置；轮转调度复用现有 Engine 调度循环与监控 Job。
- 不支持按小时轮转（仅按天）；不支持每实例 duration 权重（池内每台各占一天，按顺序循环）。
- 不做轮转池的手动「立即切换」按钮、暂停开关或切换历史页面。
- 不迁移或复用 ecs-controller（PHP 旧版）的数据库与配置；参考实现仅作行为参考。
- 不支持 Cloudflare 之外的 DDNS 提供商。
- 不修改 Android 小组件与 API Key 体系。

# 验收示例

- A1：控制台设置中出现「轮转池」标签页，可创建、编辑、删除多个轮转池；每个池可配置名称、Cloudflare API Token、Zone ID（可留空自动解析）、域名、Proxied，并关联多个已配置实例。
- A2：轮转池配置随 `/api/v1/config` 一起保存与读取；Cloudflare Token 在数据库中加密存储，读取回显时脱敏（留空保持不变）。
- A3：实例已开启「每日定时开关机」时，将其加入轮转池被拒绝并提示；实例已在轮转池中时，开启「每日定时开关机」被拒绝并提示；两个方向均在保存配置时校验。
- A4：轮转池启用且池内 ≥2 个实例时，每天到全局轮转时刻：先开机下一台实例；开机成功后轮询获取该实例公网 IPv4（每 10 秒一次、最多 12 次，PublicIpAddress 优先、EIP 兜底）；拿到 IP 后把池域名的 A 记录更新/创建为该 IPv4；DDNS 更新成功后关机其余池内实例。开机失败、IP 超时或 DDNS 失败均视为切换失败：不关机旧实例、记录错误日志并通过现有通知渠道告警。轮转位置按天推进并在进程重启后保持连续（持久化轮转状态）。
- A6（人工测试反馈）：轮转关机的池内实例不会被「抢占式实例保活」重新开机（保活逻辑感知轮转池成员身份）。
- A7（人工测试反馈）：轮转池配置页提供「立即轮转」按钮，点击后立即轮转到下一台实例（走与定时轮转相同的 Job 流程与失败保护），并显示执行结果。
- A8（人工测试反馈）：轮转池配置页提供 DDNS「测试」按钮，调用 Cloudflare API 验证 Token/Zone/域名配置是否正确，返回成功或具体错误，不修改任何 DNS 记录。

# Constraints and invariants

- 复用现有调度：轮转检查挂在 Engine 现有 15 秒 scheduler 循环内，动作通过现有 Job/Worker 执行，不新增调度器、ticker 或配置项。
- Cloudflare Token 使用现有 AES-GCM 加密体系（`master.key`）存储。
- 轮转池实例与 `schedule_enabled` 互斥为持久约束：保存配置时双向校验，运行时调度也跳过冲突实例。
- 轮转池实例同样与「抢占式实例保活」互斥：保活逻辑跳过池内实例（无论实例级或全局 keep_alive），防止轮转关机后被重新开机。
- DDNS 绑定只在开机成功后执行；绑定失败不关机旧实例（避免服务中断）。
- 公网 IP 仅接受公网 IPv4（拒绝私网/保留地址），与参考实现一致。
- 「立即轮转」与「DDNS 测试」均为手动触发操作，通过现有 Job/API 通道执行，不新增定时机制。

# Decisions
- D1：轮转粒度为「天」，池内实例按加入顺序每天轮换一台，无 duration 权重（用户需求为按天轮转；ecs-controller 的 duration/小时轮转不纳入）。
- D2：全局统一轮转时刻（用户确认）：新增全局设置「轮转时刻」（HH:MM，默认 00:00），所有池在同一时刻轮转；由现有 Engine 15 秒 scheduler 循环检测（15s 精度 + 重启补偿窗口），不新增调度器、ticker 或调度间隔设置。
- D7：切换失败才通知（用户确认）：开机失败、IP 获取超时、DDNS 更新失败时通过现有通知渠道（NotificationEvent + Outbox）发送告警；切换成功只写运行日志，不发送通知。
- D8：IP 等待策略（用户确认）：开机指令成功后每 10 秒轮询 DescribeInstances 获取公网 IPv4，最多 12 次（约 2 分钟）；超时视为切换失败，不关机旧实例。
- D9（人工测试反馈）：保活互斥在 processAccount 的 keepalive 分支实现——池内实例直接跳过保活检查；不改变 keepalive 对非池内实例的行为。
- D10（人工测试反馈）：「立即轮转」通过 POST /api/v1/rotation-pools/{id}/rotate 触发，入队一个 rotate_pool Job（uniqueKey 带时间戳允许重复手动触发），复用 runRotation 全流程与失败保护；前端按钮显示 Job 提交结果。
- D11（人工测试反馈）：「测试 DDNS」通过 POST /api/v1/rotation-pools/{id}/test-ddns 触发，后端用已存 Token 调 Cloudflare zones/records 查询 API（只读，GET），验证 Token 有效性、Zone 解析与记录可见性，返回结构化结果；不创建/更新/删除任何记录。

