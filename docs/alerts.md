# 告警配置与 Webhook 对接

在 Server JSON 配置文件顶层添加 `alerts` 对象。可将 `configs/alerts.example.json` 的内容作为这个对象。修改后重启 Server。总开关 `alerts.enabled` 默认关闭，启用后应用下表默认规则。逐规则用 `alerts.rules.<规则名>.enabled` 停用或启用，省略字段时沿用默认值。Web 的「告警」页面显示生效规则、当前告警、最近 200 条事件和接收端投递状态，支持认领、静默一小时、手动关闭及重发符合条件的失败任务。

| 规则 | 默认触发条件 | 触发 / 恢复次数 |
| --- | --- | --- |
| `cpu` | 整机 CPU 使用率 ≥ 90%，默认使用基础计算窗口 | 3 / 2 |
| `memory` | 主机内存使用率 ≥ 90% | 3 / 2 |
| `disk` | 单个可写挂载点容量使用率 ≥ 90% | 3 / 2 |
| `service_stopped` | 已配置服务上报 `stopped` | 2 / 2 |
| `host_offline` | 心跳失联且已超过 60 秒 | 1 / 2 |
| `collection_stalled` | 在线但采样序号超过 60 秒未推进 | 1 / 2 |
| `snapshot_missing` | 在线但超过 90 秒未收到新快照，实际阈值至少为三倍上报间隔 | 1 / 2 |

资源和服务规则按成功保存的不同上报点计数。默认 30 秒上报一次。CPU 可通过 `window_ms` 选择 Agent 配置的计算窗口，例如 `30000`。定时规则每 5 秒评估。Server 重启后会给 Agent 重新连接的时间。指标缺失、服务状态未知或计算窗口预热时清除连续计数，保留已触发告警，等有效数据回来再评估恢复。

可用自定义规则名配合 `metric` 新增规则，`metric` 取上表任一规则名，新规则继承对应默认值。自定义规则与内置规则独立评估。每条规则可设置 `threshold`、`trigger_count`、`recover_count`、`severity`、`host_id`、`target` 和 `receivers`。时间类阈值用秒，资源类用百分数。服务停止使用固定阈值 0，观测值 0 表示停止，1 表示运行。严重级别可用 `info`、`warning` 或 `critical`。主机和对象省略时匹配全部，`target` 用于挂载路径或服务名称、服务 key。

## 配置推送

以下为 Server 配置中 `alerts` 部分的示例。

```json
{
  "enabled": true,
  "rules": {
    "service_stopped": {"host_id": "linux-node-01", "target": "nginx", "receivers": ["ticketing"]},
    "cpu": {"threshold": 85, "window_ms": 30000, "receivers": []}
  },
  "receivers": {
    "ticketing": {
      "url": "https://alerts.example.com/monitor",
      "token_env": "MONITOR_ALERT_TOKEN",
      "timeout": "5s",
      "max_attempts": 5
    }
  }
}
```

规则省略 `receivers` 时发给全部已配置接收端，空数组时只保留站内事件。可选 `token_env` 指定 Server 进程环境中的 Bearer Token。通过 systemd 部署时可放入 Server 的环境文件。接收端 URL 支持 HTTPS 和 HTTP，默认超时 5 秒，默认最多尝试 5 次。更换接收端地址时建议使用新接收端名称，以明确旧任务对应的目的地。

## 接收契约

Monitor 对 URL 发起 POST，使用 `Content-Type: application/json` 和 `Idempotency-Key: <event_id>`。配置 Token 时携带 `Authorization: Bearer <token>`。事件包括 `event_id`、`alert_id`、`sequence`、`type`、`rule_id`、`metric`、`severity`、`host_id`、可选 `target`、`value`、`threshold` 和 UTC `occurred_at`。Webhook 推送 `FIRING` 和 `RESOLVED`。两者通过 `alert_id` 关联，事件序号递增，站内操作可能占用序号。

接收方成功持久化后返回 HTTP 2xx。超时、连接错误、408、429 和 5xx 按指数退避重试，其他状态码记为失败。任务与告警状态一起持久化，Server 重启后会恢复投递。发送中断时任务最多等待两分钟后重新可用。Webhook 采用至少一次投递语义，接收端按稳定的 `event_id` 去重并按 `sequence` 拒绝旧事件。静默期间继续评估和记录事件。触发通知受静默限制，已收到触发通知的接收端仍会收到对应恢复通知。待发送的触发任务先完成投递判定，恢复任务再按接收端的触发投递结果处理。已关闭告警的触发事件不能手动重发。

告警表由 Server 自动创建，支持 PostgreSQL 和 SQLite。当前告警规则由配置文件维护。关闭总开关会暂停评估和推送，保留已保存的事件与队列。规则停用、删除或缩小匹配范围后，Server 启动时关闭已离开评估范围的告警。完整清单确认服务或挂载点已移除时关闭相应告警。临时缺数保留已触发实例。手动关闭后，新样本仍可按规则重新触发。管理性关闭也生成 `RESOLVED` 事件，通过 `reason` 区分 `rule_changed`、`target_removed`、`host_retired` 或 `manual`。

Server 按 `retention` 清理已关闭且无待发任务的告警、事件及投递记录。活跃实例和仍有待发或发送中任务的实例会保留。
