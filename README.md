# Monitor

Linux 和 Windows 宿主机监控。两个 Go 子工程共用根目录的 Go 模块和 `protocol/monitor.proto`，可以独立构建和部署。

- `agent/` 支持 Linux 4.18 及以上和 Windows x64。Linux 版本使用 `/proc`、`/sys` 和系统接口采集指标，使用 `util-linux`、LVM2 和 `dmidecode` 补充设备信息。Agent 主动出站连接，不监听入站端口。
- `server/` 提供可选 mTLS 的 gRPC 接入和独立的 Web 管理入口。通过 SQLite WAL 或 PostgreSQL 保存主机配置版本和历史样本。Web 包含服务器列表、配置和存储拓扑、资源曲线、进程监控及按需查询。

## 构建

在指定的 Conda 环境中使用 Go 1.26 或以上版本。在工程根目录执行。

```sh
conda activate /Volumes/T9/test/envs/monitor
go test ./...
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/monitor-agent-arm64 ./agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/monitor-server-arm64 ./server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/monitor-agent-amd64 ./agent
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/monitor-server-amd64 ./server
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o bin/monitor-agent-windows-amd64.exe ./agent
```

生成的 Go 协议绑定已随代码提供。修改协议后，使用 `protoc`、`protoc-gen-go` 和 `protoc-gen-go-grpc` 重新生成。

```sh
protoc -I . --go_out=. --go_opt=paths=source_relative \
  --go-grpc_out=. --go-grpc_opt=paths=source_relative protocol/monitor.proto
```

完整的证书签发命令、JSON 参数和 systemd 安装步骤见 [安装与配置手册](docs/install.md)。

## 证书与运行

Server 的 gRPC 证书需要 `serverAuth` 用途，且 SAN 匹配 Agent 连接使用的域名或 IP。每台 Agent 单独签发 `clientAuth` 证书，包含且只包含一个 `urn:monitor:host:<host-id>` 身份 URI。Server 用受信 CA 校验证书，并比对该 URI 与 Agent 声明的主机 ID。以 `uos-01` 为例，证书 SAN 包含 `URI:urn:monitor:host:uos-01`，Agent 参数为 `-host-id uos-01`。以下用同一个 CA 演示，双方也可分别使用各自信任的 CA。

Server 默认监听 gRPC `:8443` 和 Web `127.0.0.1:8080`。先在环境中设置 `MONITOR_WEB_PASSWORD`，Web 默认用户名为 `monitor`，可通过 `-web-user` 更改。

```sh
./monitor-server-arm64 -db monitor.db \
  -tls-cert server.crt -tls-key server.key -tls-ca ca.crt

./monitor-agent-arm64 -server monitor.example.com:8443 -host-id uos-01 \
  -tls-cert uos-01.crt -tls-key uos-01.key -tls-ca ca.crt -windows 30s
```

浏览器打开 `http://127.0.0.1:8080/`，使用 Web 账号登录。默认情况下，若 Web 监听非回环地址，Server 要求配置 `-web-cert` 和 `-web-key` 启用 HTTPS；显式设置 `-web-tls-enabled=false` 可以使用 HTTP。Agent 的主机证书只用于 gRPC 接入。

## 采集、状态与保存

- Agent 默认每 **1 秒**采样，每 **5 秒**发送心跳，每 **30 秒**发送一份最新快照。额外计算窗口通过 `-windows` 指定，必须是采样周期的整数倍。速率使用实际经过时间，文件系统容量默认每 15 秒独立刷新。清单默认每 5 分钟刷新。
- Server 默认在 **15 秒**未收到有效心跳或快照时判定失联。连接断开后，Agent 退避重连并发送当前快照。断线期间的样本不补传。修改 Agent 心跳周期时，需同时检查 Server 的 `-offline-after` 设置。
- 服务器列表和详情默认读 Server 已保存的数据。按需查询经已认证的 gRPC 会话读取 Agent 内存快照，有独立超时。临时查询结果用于当前展示，历史采样仍由定时上报驱动。
- Server 以主机 ID、Agent 进程实例和采样序号对上报去重。历史默认保留 **7 天**，可用 `-retention` 调整。单次历史查询最多返回最近 3000 个样本。主机最后一次配置和最新快照可以在失联时查看。
- 分区未划分空间、VG 未分配空间和挂载点可用容量单独展示。分区、LVM 和设备信息受宿主机工具、驱动和权限影响，接口缺失时以采集状态标记。历史曲线使用中心端接收时间，并保留指标本身的采集时间和计算窗口。


## 配置文件与可选加密

Agent 和 Server 均通过 `-config` 读取 JSON 文件。文件键与命令行参数同名，允许省略参数使用默认值；未知参数、错误类型或错误时长会阻止启动。时长使用 `"1s"`、`"5m"` 等字符串，布尔值使用 JSON 的 `true` / `false`，`process-services` 使用 JSON 数组。修改文件后需要重启进程。路径相对于进程工作目录解析，生产配置建议使用绝对路径。

优先级为：**内置默认值 < 配置文件 < 指定环境变量 < 显式命令行参数**。Server 的 `MONITOR_WEB_PASSWORD`、`MONITOR_DB` 分别覆盖 `web-password`、`db`；两端的 `MONITOR_AUTH_TOKEN` 覆盖 `auth-token`。其他参数在文件或命令行配置。含密码的配置文件应设为 `0600`。

完整示例见 `configs/agent.json`、`configs/server-sqlite.json`、`configs/server-postgres.json`。修改主机 ID、地址和证书路径；设置 Web 密码。SQLite 数据文件父目录需要预先存在。

```sh
./monitor-server -config configs/server-sqlite.json
./monitor-agent -config configs/agent.json -host-id linux-node
```

两端 `tls-enabled` 默认均为 `true`，保持现有部署行为。关闭时无需任何 Agent/gRPC 证书，须两端同时设置：

```sh
./monitor-server -config configs/server-sqlite.json -tls-enabled=false -web-tls-enabled=false
./monitor-agent -config configs/agent.json -tls-enabled=false
```

`auth-token` 是可选的独立共享凭据；Server 设置后，所有 Agent 需要提供同一值。mTLS 模式仍校验每台主机的证书身份。明文模式依赖 Agent 声明的 `host-id`，共享 Token 不提供每台主机的身份隔离，数据、Token 和 HTTP Basic Auth 密码均可能以明文传输。gRPC 和 Web 的加密开关互相独立。

### Agent 全部参数

| 参数 / JSON 键 | 默认值 | 含义 |
| --- | --- | --- |
| config | 无 | 仅命令行：JSON 文件路径 |
| server | 无，必填 | Server 的 gRPC 地址 |
| host-id | machine-id，缺失则 hostname | 主机唯一 ID，最多 128 字符，允许字母数字及 `._-`，首字符为字母数字 |
| tls-enabled | true | 启用 mTLS；false 使用明文 gRPC |
| tls-cert / tls-key / tls-ca | 空 | 客户端证书、私钥、Server CA；mTLS 时必填 |
| auth-token | 空 | 可选共享 Token |
| sample-interval | 1s | 采样及最短计算窗口，必须为正 |
| windows | 空 | 附加窗口，以逗号分隔，如 `5s,30s`；各窗口为采样周期整数倍，最多 3600 个采样点，含基础窗口最多 8 个 |
| inventory-interval | 5m | 硬件与存储清单刷新周期，必须为正 |
| filesystem-interval | 15s | 文件系统容量刷新周期，必须为正 |
| push-interval | 30s | 最新快照推送周期，1s–24h |
| heartbeat-interval | 5s | 心跳周期，1s–1h；应小于 Server 的 offline-after |
| process-enabled | true | 采集已配置服务；未配置服务时不采集任何进程明细 |
| process-services | [] | 命名服务数组，最多 128 项；结构与选择器见下文 |
| process-discovery-interval | 5s | 可执行文件/命令匹配模式的 PID 发现周期，必须为正；systemd 路径异步刷新周期 |
| process-pss-enabled | false | 启用较低频率的进程 PSS 采集 |
| process-pss-interval | 30s | PSS 缓存刷新周期，必须为正 |
| process-limit | 256 | 所有目标服务合计最多上报 1–10000 个不同进程；共享进程只采集一次 |
| process-names / process-match | 空 | 旧配置兼容入口，转换成同名服务；不得与非空 process-services 同用；两者均空不代表监控全部进程 |


### Server 全部参数

| 参数 / JSON 键 | 默认值 | 含义 |
| --- | --- | --- |
| config | 无 | 仅命令行：JSON 文件路径 |
| grpc-listen | :8443 | Agent 接入监听地址 |
| web-listen | 127.0.0.1:8080 | Web/API 监听地址 |
| db-driver | sqlite | `sqlite` 或 `postgres` |
| db | monitor.db | SQLite 文件路径或 PostgreSQL DSN |
| tls-enabled | true | gRPC mTLS 开关 |
| tls-cert / tls-key / tls-ca | 空 | Server 证书、私钥、Agent CA；mTLS 时必填 |
| auth-token | 空 | 可选共享 Agent Token，设置后强制校验 |
| web-tls-enabled | true | false 强制 HTTP；true 在提供证书时使用 HTTPS，非回环监听必须提供证书；回环地址无证书保留 HTTP 行为 |
| web-cert / web-key | 空 | Web HTTPS 证书和私钥，启用时成对配置 |
| web-user | monitor | Web/API Basic Auth 用户名 |
| web-password | 空，必填 | Web/API 密码，也可通过 MONITOR_WEB_PASSWORD 提供 |
| offline-after | 15s | 心跳超时，必须大于 1s |
| retention | 168h | 历史数据保留时间，必须为正，每小时清理 |

## PostgreSQL

先由数据库管理员创建数据库和应用用户，赋予该数据库目标 schema 内的建表、建索引及读写权限。Server 自动创建不存在的表和索引，支持重复启动；不会自动创建数据库、迁移 SQLite 历史或复制两类数据库之间的数据。

```sh
MONITOR_DB='postgres://monitor:YOUR_PASSWORD@db.example.com:5432/monitor?sslmode=verify-full&sslrootcert=/path/db-ca.crt' \
  ./monitor-server -config configs/server-postgres.json
```

DSN 使用 pgx 支持的 URI 或键值格式，数据库连接是否加密由 DSN 的 `sslmode` 独立决定。示例配置中的 `sslmode=disable` 只演示本机测试。PostgreSQL 采用 `BYTEA` 保存原始 JSON 字节，时间和采样 ID 使用 `BIGINT`，与 SQLite 提供相同的配置版本、去重、最新值、历史查询和保留清理行为。

数据库回归测试默认运行 SQLite。提供有创建 schema 权限的测试 DSN 后，可运行 PostgreSQL 真库测试；它使用临时独立 schema，结束后删除该测试 schema：

```sh
MONITOR_TEST_POSTGRES_DSN='postgres://user:password@127.0.0.1:5432/testdb?sslmode=disable' \
  go test ./server -run TestPostgresRoundTrip -count=1 -v
```

## 服务与进程监控

以业务服务为配置和展示单位，**默认不监控任何进程**。在 `process-services` 中定义“自定义名称 + 进程识别方式”；启动命令拆成可执行文件路径和关键参数，Agent 只识别和监控，不执行该命令或自动拉起服务。

```json
{
  "process-enabled": true,
  "process-discovery-interval": "5s",
  "process-services": [
    {
      "name": "订单服务",
      "match": {
        "executable": "/usr/bin/java",
        "args_contains": ["-jar", "/opt/orders/orders.jar"]
      },
      "include_children": true
    },
    {
      "name": "Redis",
      "match": {"systemd_unit": "redis-server.service"},
      "include_children": true
    }
  ]
}
```

完整示例见 [agent-services.example.json](configs/agent-services.example.json)，其中路径、参数、unit、cgroup 均需按实际服务修改。每项名称唯一且不超过 128 字节。需要把多个启动方式归到同一服务时，可用 `match_any` 替代 `match`，列出 1 至 32 组 executable 和可选 args_contains，命中任一组即归入该服务。使用 `match` 时恰好配置一种选择器：

- `systemd_unit`：指定 `.service` 单元。通过只读 systemctl 查询其实际 ControlGroup，异步执行以免阻塞 1s 指标采集；不假定服务位于 system.slice。单元停止时无成员，查询失败时状态未知。
- `cgroup`：指定内核层级中的绝对路径，如 `/system.slice/orders.service`，不是 `/sys/fs/cgroup/...` 文件系统路径。根据挂载信息适配 cgroup v1/v2。禁止选择整个根 cgroup。
- `executable` + 可选 `args_contains`：通过 `/proc/PID/exe` 校验可执行文件（支持解析配置路径符号链接），关键参数均须以完整 argv 项出现，不要求相邻。多个根进程可属于同一服务。匹配范围决定了服务实例归属，应选足够明确的参数。

`include_children` 默认 true：cgroup 模式包含下级 cgroup；命令模式包含后代进程，并保留已识别后代在父进程退出、重新归属后的服务关联，直到该进程退出。false 时 cgroup 只枚举本组直接成员，命令模式只包含匹配根进程；此时资源汇总使用对应进程，不使用包含下级组的 cgroup 统计。

命令模式仅在发现周期枚举 PID 和匹配元数据，详细指标仅对目标进程采集。cgroup 模式每次采样从目标组获取成员，无需枚举所有系统进程。Agent 探测到 pidfd 接口可用时用它辅助检测退出，否则使用 procfs。上游 Linux 5.3 引入该接口，实测两台 UOS 4.19 测试机也提供该能力；所有模式仍用 PID + start_ticks 校验身份。命令模式中新进程可能在下一次发现时才出现，快速创建并退出的进程可能不被观察到。

Web 以服务展示状态、进程数、CPU、RSS 合计、可选 PSS、cgroup 内存、线程/FD 合计及 I/O，展开显示当前进程树。同一进程在多个服务匹配时明细只保存一次，各服务引用它；不同服务合计不能直接相加。CPU/IO 计算窗口沿用 sample-interval/windows，推送周期不改变窗口定义；服务 CPU 同样以单核为 100%，可能超过 100%。

服务 CPU/I/O 优先使用可用 cgroup 内核累计计数，成员变化时仍能包含已退出进程的组内资源消耗。无对应接口则汇总进程速率，但只在整个计算窗口成员集合不变、计数连续且完整时给出合计，否则缺失并附 service_status。明细被 process-limit 截取时，不用部分 RSS/线程/FD 冒充完整汇总；cgroup 原生指标仍可独立使用。混合层级不混用 v1/v2 计数；v1 不同控制器组的成员不一致时也退回进程统计。

RSS 合计可能重复计算共享内存，与 `cgroup_memory_bytes` 的内核记账口径不同。可选 PSS 优先读取 smaps_rollup，缺失时读取 smaps；默认关闭，开启后默认每 30s 刷新，并标注采集时间。PSS 是按比例分摊共享内存，不等于服务 cgroup 内存。JVM 显式 -Xmx、累计 read_bytes/write_bytes 及进程线程/RSS/CPU 保留旧 Shell 脚本参考口径，原脚本固定网络 0 不作为指标。

服务状态为 running/stopped/degraded/unknown，表示进程存在和采集情况，不代表业务探活健康。`observed_starts/exits/restarts` 仅累计本次 Agent 运行观察到的变动，不是系统历史精确计数；首轮已有进程不计作新启动。重启依据命令根进程整批替换、已观察到停止后重新出现，或 cgroup 身份变更判断；仅父进程退出后子进程重新归属不算重启。Agent 不接管启动、停止或自动重启。

第三方接入协议、服务/进程字段口径和交互示例见 [Agent 数据对接说明](docs/agent-integration.md)。

## 告警

Server 支持站内告警、认领、静默和第三方 Webhook。使用 JSON 的 `alerts` 配置总开关、默认阈值覆盖和推送接收端。详见 [告警配置与 Webhook 对接](docs/alerts.md)。
