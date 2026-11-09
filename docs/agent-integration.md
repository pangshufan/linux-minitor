# Agent 数据对接说明（monitor.v1）

本文面向第三方监控平台。协议定义以工程内 `protocol/monitor.proto` 为准；Go 绑定已生成，其他语言可使用该文件生成 gRPC 客户端/服务端。JSON 采集结构定义位于 `agent/metrics.go`、`agent/processes.go` 和 `agent/inventory.go`。

## 1. 接入方式

有两种方式：

1. **直接接收 Agent**：第三方实现 `monitor.v1.Monitor/Connect` 双向流式 gRPC 服务，将 Agent 的 `server` 配置到该地址。第三方负责身份校验、会话管理、持久化和在线判定。每个 Agent 当前只连接一个中心地址。
2. **通过本系统 Server API 集成**：保留现有 Agent → Server 链路，第三方使用 Server 的 HTTP API 查询主机列表、详情、历史与实时快照。无需改动 Agent，适合已有中心端和多消费者的情况。

Agent 只主动出站连接，不监听入站端口。主动查询通过已建立的流反向下发，只允许查询最新缓存和配置清单，不提供命令执行。

## 2. 传输和认证

gRPC 默认端口 `8443`，Web 默认 `127.0.0.1:8080`，可配置。

| 模式 | Agent / Server 配置 | 平台需实现 |
| --- | --- | --- |
| mTLS，默认 | 两端 `tls-enabled=true`，配置 tls-cert/tls-key/tls-ca | 验证客户端证书、可信 CA 和主机身份；Server 证书须匹配连接域名/IP |
| 明文 gRPC | 两端 `tls-enabled=false` | 使用 HTTP/2 明文传输，验证 Hello 的 host_id 格式；不具有证书主机身份绑定 |
| 可选共享 Token | 两端配置 auth-token，或 MONITOR_AUTH_TOKEN | 读取初始 gRPC metadata 的 `authorization: Bearer <token>`，校验失败拒绝连接 |

mTLS 支持 TLS 1.2 及以上。每个 Agent 证书包含 `clientAuth`，且其 URI SAN 中只允许一个主机身份 `urn:monitor:host:<host-id>`，必须与 Hello.host_id 相同。Token 可与 mTLS 叠加；明文模式中的 Token 也是明文，不能代替每主机身份校验。Token 并非 HTTP Web 密码，两套凭据独立。

`host_id` 最长 128 字符，首字符为英文字母或数字，后续允许字母、数字、`.`、`_`、`-`。同一中心只允许同一 host_id 的一个活动会话。部署多台 Agent 时必须分配不同 host_id。

本实现两端 gRPC 消息上限为 **32 MiB**；第三方至少配置一致的接收上限，并限制解析资源。JSON bytes 在原生 gRPC 绑定中直接为 UTF-8 字节；只有转换为 Protobuf JSON 表示时才使用 Base64。

## 3. 流式交互顺序

RPC 全路径：`/monitor.v1.Monitor/Connect`。

```text
Agent                               第三方接收端
  ── Connect（TLS / 明文，metadata） ──▶
  ── Hello ──────────────────────────▶  校验身份、登记会话
  ◀────────────────────────────── Ack
  ── Snapshot（已有的最新缓存） ──────▶  允许启动阶段只有清单或只有指标
  ── Heartbeat ──────────────────────▶
  ◀────────────────────────────── Ack
  ── Snapshot（每 push-interval） ────▶
  ◀──────────────────────────── Query  可随时请求缓存
  ── QueryResult ────────────────────▶  使用 request_id 对应请求
```

Hello 必须为第一条 Agent 消息，后续重复 Hello 视为协议错误。接收端需要串行写入同一流（例如使用发送队列），并持续读取。Agent 启动时采集器和连接并发运行，初始快照可能暂无指标或清单；平台应接受这种预热状态。

**Ack 在注册和每次有效 Heartbeat 后发送**，不是逐样本持久化确认。Agent 若连续超过 3 个 heartbeat-interval 未收到 Ack，会主动断开并重连。仅接收 Snapshot 而不回复心跳 Ack 将导致重连。

默认心跳 5s，中心端 offline-after 15s；自行调整时应保留调度和网络抖动余量。Server 收到有效心跳或快照更新活跃时间；流断开立即使该主机离线。TCP 仍连接不代表采集正常，应用层应区分在线和采集是否推进。

Agent 重连退避约为 0.5–30s，具有随机抖动。重连只发送当前缓存，**不重放、不补传断线历史**。

## 4. 消息定义

### Hello

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| host_id | string | 稳定主机 ID |
| run_id | string | Agent 每次启动生成的 16 字节随机值，编码为 32 个十六进制字符；同一进程重连不变 |
| report_interval_ms | int64 | 推送周期，1000–86400000 ms；不等于计算窗口 |

指标去重键为 `(host_id, run_id, metrics.sample_id)`。sample_id 是本次 Agent 运行内递增序号，重启从头计数；断线后序号可能跳跃。这不是丢包重传协议。

### Heartbeat / Ack

Heartbeat.latest_sample_id 为最新采样序号（uint64），启动未有采样时为 0。序号持续推进说明采集器正在运行，即使 push-interval 很长，也不应据此认为采集停滞。Ack 是空消息。

### Snapshot

三个字段均为 UTF-8 JSON 字节，可独立缺失：

| 字段 | 内容 | 刷新 / 推送 |
| --- | --- | --- |
| metrics_json | 指标与进程快照 | 默认每 1s 刷新，每 30s 推送最新一份 |
| filesystems_json | 挂载列表与容量 | 默认每 15s 刷新，随指标推送 |
| inventory_json | 系统、硬件、块设备、LVM 清单 | 默认每 5m 刷新；首次及缓存刷新后的下一次推送、重连时携带 |

快照至少包含 metrics_json 或 inventory_json，filesystems_json 随指标携带。缺失字段代表本条消息没有携带该数据，**不代表清空之前保存的数据**。每份 JSON 都有自己的 collected_at，应分别评估新鲜度。接收端不应要求三个采集时间相同。

本系统每个定时快照只保存一份最新值，并不打包这 30 秒内所有 1 秒样本。30s 推送中的 1s 窗口依然表示最近约 1s 的资源速率，不能标为最近 30s 平均值。

### Query / QueryResult

Query.request_id 为平台生成的唯一关联 ID，Query.kind：

| 枚举 | 数值 | 返回 |
| --- | --- | --- |
| KIND_LATEST | 1 | 最新 metrics_json 和 filesystems_json |
| KIND_INVENTORY | 2 | 最新 inventory_json |
| KIND_UNSPECIFIED / 未知值 | 0 / 其他 | QueryResult.error 为 unsupported query |

Agent 原样返回 request_id。成功时有 snapshot；失败时有 error，且无 snapshot。指标或清单未就绪时返回 warming up 类错误。查询读取最新内存缓存，不会临时执行一轮新采集。第三方需自行实现超时、取消、关联表容量限制；本系统 HTTP 查询超时 3s，每主机最多 8 个待完成查询。查询结果只用于返回给调用方，本系统不把它额外插入历史。

## 5. 指标 JSON

以下为节选示例，非完整必填模板：

```json
{
  "sample_id": 42,
  "collected_at": "2026-11-08T12:00:00Z",
  "sample_interval_ms": 1002.4,
  "booted_at": "2026-11-01T00:00:00Z",
  "uptime_seconds": 648000.2,
  "load_average_1m_5m_15m": [0.2, 0.3, 0.4],
  "memory_available_source": "kernel",
  "memory_bytes": {"total": 8589934592, "available": 6442450944, "used": 2147483648},
  "gauges": {"system": {"procs_running": 2}},
  "windows": [{
    "requested_ms": 1000,
    "window_start": "2026-11-08T11:59:58.9976Z",
    "window_end": "2026-11-08T12:00:00Z",
    "elapsed_ms": 1002.4,
    "status": "ok",
    "cpu_percent": {"cpu": {"busy": 12.5, "idle": 85.5, "iowait": 2.0}},
    "rates": {
      "disk/8:0/sda": {"read_bytes": 4096.0, "write_bytes": 8192.0},
      "net/eth0": {"rx_bytes": 1024.0, "tx_bytes": 2048.0},
      "process/123/456": {"cpu_percent": 25.0, "cpu_time_ns": 250000000.0, "read_bytes": 4096.0}
    }
  }]
}
```

`collected_at`、`window_start/end` 为带时区的 RFC3339 时间，应接受小数秒与时区偏移。`sample_interval_ms` 是相邻采样的实际经过时间，首样本为 0。采集基于各接口顺序读取，不是整个系统的原子快照。

### 计算口径

- `windows[].requested_ms` 为配置窗口，`elapsed_ms` 为实际跨度。所有速率按累计计数差 / 实际秒数计算；跨重置、缺项或新进程首次出现时，相应速率字段缺失。
- 窗口 `status=insufficient_samples` 表示尚无足够样本；此时不得把缺失值渲染为 0。`status=ok` 仅代表有可计算的时间跨度，各个字段仍可能缺失。
- windows[0] 始终为 sample-interval 的基础窗口，额外窗口按配置顺序出现；消费者应按 requested_ms 查找，避免固定第二项就是 30s。
- 整机 CPU 为 `cpu_percent.cpu.busy`，0–100%；`cpu0/cpu1/...` 为单核。busy = 100 - idle - iowait，guest 不重复计入总量。其他分类包括 user/nice/system/irq/softirq/steal。
- memory_bytes 使用字节，used = total - MemAvailable；swap_used = swap_total - swap_free。可用内存来源由 memory_available_source 标识，不应自行等同为 MemFree。
- gauges 是瞬时计数值，例如运行/阻塞进程数、已分配文件句柄、磁盘 io_in_progress、TCP established_or_close_wait。负载平均值已明确为 1/5/15 分钟内核指标，不能当作 1s CPU 使用率。

### rates 分组和单位

| 分组 / 字段 | 单位 / 含义 |
| --- | --- |
| system / ctxt, intr, processes | 次/s：上下文切换、中断、新建进程 |
| vm / 内核 vmstat 字段名 | 次/s 或页/s，保留内核字段语义，详见下文 |
| disk/major:minor/name / read_bytes, write_bytes | bytes/s，内核 512 字节扇区计数换算 |
| disk/... / reads, writes | IOPS |
| disk/... / read_time_ms, write_time_ms | 累计毫秒/s；不是直接的平均延迟或百分比 |
| disk/... / discards, flushes, discard_bytes | 丢弃/刷新次数每秒、丢弃 bytes/s；按内核接口可用性提供 |
| net/iface / rx_bytes, tx_bytes | bytes/s |
| net/iface / rx_packets, tx_packets, rx_errors, tx_errors, rx_drop, tx_drop | packets 或 events /s |
| tcp, tcpext, udp / 保留内核字段名 | 次/s，例如 RetransSegs、ListenDrops |
| psi/cpu, psi/memory, psi/io / some_stall_us, full_stall_us | 微秒/s，可除以 10000 换算压力百分比；按接口存在情况提供 |
| process/pid/start_ticks | 进程速率，见下一节 |

VM 当前采集 pgmajfault、pswpin、pswpout；pgmajfault 为 events/s，pswpin/pswpout 为 pages/s，后者需要结合宿主机页大小解释，不能直接当作 bytes/s。磁盘速率不能把底层设备、分区、dm/LV 全部相加，否则会重复统计同一 I/O。

## 6. 命名服务与进程 JSON

Agent 只采集配置中的服务。`process-services` 是包含 name、match 或 match_any、include_children 的数组；match 选择 systemd_unit、cgroup 或 executable + args_contains，match_any 用于归并多个 executable + args_contains 匹配规则，详见 README 和 configs/agent-services.example.json。未配置目标时 processes.services/items 均为空，不再默认监控所有系统进程。配置变更需重启 Agent。

`metrics_json.processes` 示例（节选）：

```json
{
  "enabled": true,
  "clock_ticks_per_second": 100,
  "lifecycle_source": "pidfd+procfs",
  "matched": 2,
  "reported": 2,
  "truncated": false,
  "matches": {"orders": 2},
  "services": [{
    "key": "service/orders",
    "name": "orders",
    "state": "running",
    "membership_source": "cgroup",
    "cgroup": "/system.slice/orders.service",
    "process_keys": ["process/123/456", "process/124/457"],
    "root_keys": ["process/123/456"],
    "process_count": 2,
    "reported_count": 2,
    "root_count": 1,
    "complete": true,
    "threads": 8,
    "rss_sum_bytes": 67108864,
    "cgroup_memory_bytes": 75497472,
    "fd_sum": 30,
    "metric_sources": {"cpu":"cgroup_v2", "io":"cgroup_v2", "memory":"cgroup_v2", "rss_sum":"procfs"},
    "observed_starts": 0,
    "observed_exits": 0,
    "observed_restarts": 0
  }],
  "items": [{
    "key": "process/124/457",
    "pid": 124,
    "ppid": 123,
    "parent_key": "process/123/456",
    "service_keys": ["service/orders"],
    "name": "worker",
    "state": "S",
    "uid": 1000,
    "threads": 4,
    "start_ticks": 457,
    "rss_bytes": 33554432,
    "virtual_bytes": 134217728,
    "memory_percent": 0.390625,
    "fd_count": 12,
    "io_counters": {"read_bytes": 4096, "write_bytes": 8192}
  }]
}
```

实际服务 key 为 `service/` + URL PathEscape(name)，例如名称 orders 的 key 是 service/orders；消费者直接使用返回的 key，不应自行根据显示名称猜测编码。示例进程明细省略了父进程。Process key 仍为 PID + start_ticks；跨主机/Agent 运行还需结合 host_id、run_id，跨系统启动可结合 inventory.system.boot_id。

### 服务归属、层级和状态

| 字段 | 语义 |
| --- | --- |
| membership_source | cgroup 或 process_tree；命令匹配模式的归属为进程树推断 |
| process_keys / items[].service_keys | 服务到进程及进程到服务的引用；同一进程可以属于多个显式配置的服务，但 items 中只出现一次 |
| parent_key | 当前已采集进程集合内经过 start_ticks 验证的父进程身份；不一定属于同一个服务 |
| root_keys | 该服务成员集合中当前没有父成员的进程，用于构建森林；不等于业务副本数量 |
| process_count / reported_count | 当前识别的存活进程数 / 实际包含明细数，不含僵尸；截取或退出竞态时可以不同 |
| complete | 成员与明细是否完整；false 时 RSS/线程等完整服务合计缺失 |
| state | running 存在成员；stopped 未发现成员且采集成功；degraded 存在成员但采集不完整或命令根已退出而后代仍存活；unknown 无成员且无法确认采集成功 |
| lifecycle_source | procfs 或 pidfd+procfs；后者通过可用 pidfd 辅助发现退出，仍定期发现新进程并校验身份 |
| observed_starts/exits/restarts | 本次 Agent 生命周期内观察到的计数，不是历史完整事件日志，不保证捕获采样间隔中的短命进程 |

进程树与服务归属分离：cgroup 模式包含目标组及可选下级组；父进程退出不会让仍在组内的子进程丢失归属。命令模式默认记录匹配根及其后代，对已识别后代重新归属后保留关联；新后代受 process-discovery-interval 影响。Agent 初次启动时已经脱离原父进程的孤儿进程，无法仅靠命令根推断归属，此类服务宜使用 cgroup。

只在服务内按 parent_key 关联树，找不到父成员时作为根；允许多个根，并防御瞬时缺失或循环异常。界面应通过服务名显示业务含义，通过 PID/start_ticks 显示进程实例。不要把多个相互重叠服务的汇总再次相加。

observed_starts/exits 反映服务成员进入或离开，不保证代表系统层进程创建或死亡。首轮已存在进程不计 observed_starts。observed_restarts 依据已观察到停止后重新出现、命令匹配根集合整体替换或 cgroup 身份改变计算；子进程重新归属不单独算作重启。进程存在不等于业务健康；本功能不执行启动命令、不自动拉起服务。

### 服务指标与窗口

服务速率放在 `windows[].rates[service.key]`，字段为 cpu_time_ns、cpu_percent、read_bytes、write_bytes。CPU 以一个逻辑核为 100%，read/write 为 bytes/s。`windows[].service_status[service.key]` 针对 cpu_time_ns/read_bytes/write_bytes 标注：

- ok：可计算；
- insufficient_or_reset：对应源没有连续的可用累计计数，可能为首次采样或重置；
- membership_changed_or_incomplete：进程汇总窗口成员变化或明细不完整。

窗口不足时仍使用既有 windows.status=insufficient_samples，此时可能没有 service_status。缺失值不得转成 0。

默认 include_children=true 时，CPU 和 I/O 优先使用可用的 cgroup v2 cpu.stat/io.stat，或 v1 cpuacct.usage/可用递归 blkio 统计。通过挂载信息、接口存在性、权限选择来源，不单凭内核版本断言支持。混合挂载时不从另一版本层级借用统计；v1 不同控制器组的成员不一致时退回进程汇总。原生组累计计数允许成员变化且保留已退出进程的组内消耗；无对应接口时退回 procfs_sum，仅在整个窗口成员集合稳定、计数连续、明细完整时求和。include_children=false 使用直接成员的进程汇总，避免混入下级组的内核统计。

`metric_sources` 分别说明 cpu/io/memory/rss_sum 的来源。cgroup_memory_bytes 是内核组内存记账，v1 记账还受控制器层级设置影响；rss_sum_bytes 是进程 RSS 合计，可能重复计算共享页，不能当作无重复物理内存。两者不是相同指标。FD 是各进程 FD 数合计，同样不是去重的内核文件对象数。

可选 process-pss-enabled 开启后，优先从 smaps_rollup、缺失时从 smaps 获取 PSS，按 process-pss-interval（默认 30s）缓存。进程项包含 pss_bytes/pss_at，服务包含 pss_sum_bytes/pss_oldest_at，后者标记成员 PSS 中最旧的采集时间。PSS 不使用 1s 窗口冒充实时值。无法取得所有成员 PSS 时不生成服务 PSS 合计。

### Shell 脚本参考指标

保留 PID、threads、rss_bytes、窗口 cpu_percent、累计 io_counters.read_bytes/write_bytes、显式 jvm_max_heap_bytes/jvm_max_heap_option。额外提供 PPID、UID、状态、虚拟内存、内存占比、FD、启动时间、运行时长、缺页与上下文切换速率。脚本原先固定为 0 的网络列不输出。

进程 CPU 使用 AT_CLKTCK 换算，不假定 100 Hz；窗口 cpu_percent = cpu_time_ns / 10000000。IO 中 rchar/wchar 是系统调用层字符数，syscr/syscw 是调用次数，不同于存储 read_bytes/write_bytes。minor_faults/major_faults/voluntary_context_switches/involuntary_context_switches 的速率为 events/s。

-Xmx 提取完整参数，支持无单位字节或 K/M/G/T，大小写不敏感，重复时取最后一个可解析值；未显式指定就缺失，不猜测 JVM 默认值，不采集真实堆使用量。原始命令行不会上报。

process-limit 对所有目标服务的不同进程明细设置总上限，优先按 CPU 增量、RSS、PID 排序；超限不改变服务归属，只让服务明细不完整。cgroup 原生资源计数与明细截取独立，仍可有效。processes.matched/reported/truncated 是全局目标进程数量，matches 是按服务名称的成员数；各服务可能重叠。

没有配置服务、服务已停止、缺少权限或接口、被截取是不同状态。读取失败会以 issues/缺失字段反映，不使用零模拟。系统仍以 Linux >= 4.18 为基线；pidfd_open 根据运行时探测启用，否则退回 procfs。上游 Linux 5.3 引入该接口，发行版可能回移到较早内核。PSS 等接口同样依赖内核编译选项与权限。

## 7. 配置清单与文件系统

inventory_json 顶层字段：host_id、collected_at、system、hardware、block_devices、lvm、lvm_status、issues。

| 字段 | 内容与约定 |
| --- | --- |
| system | hostname、id/version_id/pretty_name、kernel_release、architecture、boot_id、booted_at；值为字符串 |
| hardware.cpu | 型号、可见逻辑 CPU / 核 / 插槽、在线列表等，按能力提供 |
| hardware.dmi / memory_devices / network_interfaces | 厂商机型序列号、DIMM、网卡和驱动等 |
| block_devices | 以 major:minor 为键，含 path/name/kind/size_bytes、parents、型号序列号、分区表、分区类型和内容签名等 |
| block_devices[].parents | 指向该块设备的底层设备，不是挂载点路径 |
| unpartitioned | 磁盘未划分空间，bytes 总量与 regions；regions 的 end_sector_inclusive 为包含末端扇区 |
| lvm | physical_volumes、volume_groups、logical_volumes、logical_volume_segments；来自 LVM 工具，数值通常为十进制字符串，容量为字节字符串，需允许小数 |
| lvm_status | ok / partial / unavailable；不能因缺失 lvm 就认定主机没有 LVM |

未划分空间（分区表）、VG 剩余空间（LVM）和挂载点可用容量（文件系统）属于不同层次，不能相互替代。block_devices 中 start_sector_512_bytes 使用 512 字节单位；unpartitioned.regions 使用其 sector_bytes 指定的扇区大小。

filesystems_json 包含 collected_at、mounts，异常时含 issue。每个挂载项有 mount_id、device（major:minor）、root、target、fstype、source、read_only，以及可选 capacity、capacity_at、capacity_issue。

capacity 的 total_bytes、used_bytes、available_bytes、total_inodes、free_inodes 来自 statfs。available_bytes 是非特权进程可用空间，因此未必等于 total_bytes-used_bytes。跳过的远程、虚拟或未知文件系统保留挂载项和 capacity_issue，容量字段缺失。

## 8. 使用本系统 Server HTTP API

Web/API 加密由 web-tls-enabled/web-cert/web-key 配置，独立于 gRPC。除 `/healthz` 外所有端点要求 HTTP Basic Auth。响应为 JSON；错误形式为 `{"error":"..."}`。

| 请求 | 返回 |
| --- | --- |
| GET /healthz | 进程 HTTP 健康状态，不能代替数据库/Agent 健康检查 |
| GET /api/hosts | 主机摘要数组，含 host_id/hostname/online/collection_status/booted_at/资源利用率 |
| GET /api/hosts/{host_id} | 主机详情，含 metrics/filesystems/inventory 最新保存对象及其接收/采集时间 |
| GET /api/hosts/{host_id}/history?range=1h | 最多 3000 个历史点，按接收时间升序；range 为大于 0 且不超过 168h 的 Go 时长。可选 since_ms 为包含边界的接收时间（Unix 毫秒），与 range 共同限制查询范围 |
| POST /api/hosts/{host_id}/query/latest | 通过现有流获取 Agent 最新缓存，返回 metrics/filesystems |
| POST /api/hosts/{host_id}/query/inventory | 获取 Agent 清单缓存，返回 inventory |

示例（本机 HTTP）：

```sh
curl --user monitor http://127.0.0.1:8080/api/hosts
curl --user monitor http://127.0.0.1:8080/api/hosts/linux-node
curl --user monitor 'http://127.0.0.1:8080/api/hosts/linux-node/history?range=1h'
curl --user monitor -X POST -H 'Content-Type: application/json' \
  http://127.0.0.1:8080/api/hosts/linux-node/query/latest
```

curl 将交互式询问密码。HTTPS 使用可信 CA（例如 curl --cacert），以实际部署地址替换示例 URL。POST 查询要求 Content-Type 精确为 application/json，不需要请求体。

详情包含 last_seen_ms、latest_received_ms、inventory_ms、report_interval_ms 等 Unix 毫秒时间。online 来自当前活动会话与心跳；重启 Server 后保存的旧主机应为离线，直到 Agent 重连。collection_status 为 active / stalled / warming_up / unknown，在线不代表指标正在推进。

历史点包含 collected_at、received_ms、window_ms、elapsed_ms、cpu_percent、memory_percent、swap_percent、rates、filesystems、inventory_version。rates 是基础计算窗口的速率，包含进程和服务分组；额外返回 services 服务汇总与 service_status，可查询服务 RSS/CPU/I/O 趋势。历史接口不返回完整 processes.items，也不返回额外计算窗口，不能据此还原每个历史进程的 RSS 或 Xmx。完整原始快照保存在数据库中，API 扩展应另行约定，不建议第三方直接依赖内部表结构。切换数据库不会改变 HTTP API。

查询常见错误：401 未认证；404 未知主机详情；409 主机不在线；415 缺少指定 Content-Type；429 待查询队列满；502 Agent 返回错误或响应无效；504 查询超时。history 对未知主机可能返回空数组。接口不提供跨页历史导出。

## 9. 兼容性与集成检查

JSON 字段采用可加性演进，消费者应忽略未知字段，保留未知设备类型/分组，处理缺失与 null，不以字段顺序解析。整数计数可能超过 JavaScript 安全整数范围，若必须无损保存 uint64，应使用支持大整数的 JSON 解析器；本协议不会把 JSON uint64 自动转为字符串。

本系统按去掉 collected_at 后的清单内容计算配置版本。第三方可使用自己的版本算法，但不要将清单采集时间变化误当配置变化。不要将启动预热、采集权限失败或不支持接口转换为 0。

建议验收：两台主机分别注册；重复主机 ID 被拒绝；Token/证书错误被拒绝；1s 与 30s 窗口显示不同口径；心跳持续但样本序号不变能标记采集停滞；断网后离线且重连不补传；Agent 重启后同 sample_id 不与旧 run_id 冲突；PID 复用不产生跨进程速率；无业务进程、缺少 LVM 工具或无法读取 io 时正确显示缺失状态。

## 10. Python 最小接收示例

以下示例用于隔离测试中的明文接收，演示 Hello/Ack、心跳和 JSON 解码。它没有生产所需的持久化、重复会话管理或反向查询队列；部署 mTLS 时须按第 2 节验证客户端证书身份。

先安装 grpcio/grpcio-tools，并从工程根目录生成 Python 绑定：

```sh
mkdir -p generated
python -m grpc_tools.protoc -I protocol --python_out=generated \
  --grpc_python_out=generated protocol/monitor.proto
```

将以下代码保存为 generated/receiver.py，运行 `python generated/receiver.py`，并配置 Agent：`-server 127.0.0.1:8443 -tls-enabled=false`（远程 Agent 需改用可达的监听地址）。

```python
import asyncio
import json
import os
import re
import secrets

import grpc
import monitor_pb2 as pb
import monitor_pb2_grpc as rpc


class Receiver(rpc.MonitorServicer):
    async def Connect(self, messages, context):
        expected = os.environ.get("MONITOR_AUTH_TOKEN", "")
        values = [v for k, v in context.invocation_metadata() if k == "authorization"]
        if expected and (len(values) != 1 or not secrets.compare_digest(
                values[0], "Bearer " + expected)):
            await context.abort(grpc.StatusCode.UNAUTHENTICATED, "invalid token")
        hello = None
        async for message in messages:
            kind = message.WhichOneof("body")
            if hello is None:
                if kind != "hello":
                    await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "Hello required")
                hello = message.hello
                if (not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}", hello.host_id)
                        or not re.fullmatch(r"[0-9a-fA-F]{32}", hello.run_id)
                        or not 1000 <= hello.report_interval_ms <= 86400000):
                    await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "invalid Hello")
                yield pb.ServerMessage(ack=pb.Ack())
            elif kind == "heartbeat":
                # Update liveness and latest_sample_id in your platform.
                yield pb.ServerMessage(ack=pb.Ack())
            elif kind == "snapshot":
                try:
                    payload = {
                        name: json.loads(raw)
                        for name in ("metrics_json", "filesystems_json", "inventory_json")
                        if (raw := getattr(message.snapshot, name))
                    }
                except (ValueError, UnicodeDecodeError):
                    await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "invalid JSON")
                metrics = payload.get("metrics_json", {})
                # Validate/persist payload; deduplicate by host_id, run_id, sample_id.
                print(hello.host_id, hello.run_id, metrics.get("sample_id"))
            else:
                await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "unexpected message")


async def main():
    server = grpc.aio.server(options=[
        ("grpc.max_receive_message_length", 32 << 20),
        ("grpc.max_send_message_length", 32 << 20),
    ])
    rpc.add_MonitorServicer_to_server(Receiver(), server)
    server.add_insecure_port("127.0.0.1:8443")
    await server.start()
    await server.wait_for_termination()


asyncio.run(main())
```

示例中的进程打印不是持久化确认。完整平台还需按前文实现连接超时、数据校验、在线/采集状态、队列容量和资源限制。
