# 安装与配置

Agent 支持 Linux 4.18 及以上，提供 AMD64 和 ARM64 包。Server 提供 Linux AMD64、Linux ARM64 和 Windows AMD64 包，内置 Web 页面，支持 SQLite 和 PostgreSQL。GitHub Actions 的 **Build packages** 工作流会在 push、PR 和手动触发时生成归档，产物可在对应运行的 **Artifacts** 中下载。Windows 包为 ZIP，Linux 包为 tar.gz。Linux 包中附带对应的 systemd 单元。

## 选择加密方式

| 链路 | 启用方式 | 关闭方式 |
| --- | --- | --- |
| Agent → Server gRPC | 两端设 `tls-enabled: true`，分别提供证书、私钥和受信 CA。Server 校验 Agent 证书中的主机身份 | 两端设 `tls-enabled: false`，无需 gRPC 证书 |
| 浏览器 → Server Web | Server 设 `web-tls-enabled: true`，同时配置 `web-cert` 和 `web-key` | 设 `web-tls-enabled: false` 使用 HTTP。默认回环监听在没有 Web 证书时也使用 HTTP |
| Server → PostgreSQL | DSN 设置 `sslmode=verify-full` 及 `sslrootcert`。数据库证书的 SAN 需匹配 DSN 的主机名 | DSN 显式设置 `sslmode=disable`，适用于受控测试网络 |

三个开关各自生效。Web Basic Auth 在 HTTP 下会以明文传输。Agent 共享 `auth-token` 可在两端同时配置，明文 gRPC 下 Token 同样可被链路上的第三方读取。关闭 mTLS 后，Server 使用 Agent 声明的 `host-id`。

## 签发证书

在签发机上运行以下命令，示例使用 OpenSSL 1.1.1 或更新版本。将 `monitor.example.com` 和 `192.0.2.10` 换成实际 Server 的域名和 IP。Agent 以 IP 连接时证书必须包含对应 IP SAN。以下演示用同一 CA 签发三种证书，可按环境拆分信任链。

```sh
umask 077
mkdir monitor-pki
cd monitor-pki

openssl req -x509 -newkey rsa:3072 -nodes -sha256 -days 3650 \
  -subj '/CN=Monitor Root CA' -keyout ca.key -out ca.crt \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'subjectKeyIdentifier=hash'

cat > server.ext <<'EXT'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
subjectAltName=DNS:monitor.example.com,IP:192.0.2.10
EXT

issue() {
  name=$1
  extensions=$2
  openssl req -new -newkey rsa:2048 -nodes \
    -subj "/CN=$name" -keyout "$name.key" -out "$name.csr" &&
  openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key \
    -CAcreateserial -sha256 -days 365 -extfile "$extensions" -out "$name.crt"
}

issue server server.ext
issue web server.ext

host_id=linux-node-01
cat > agent.ext <<EXT
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=clientAuth
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
subjectAltName=URI:urn:monitor:host:$host_id
EXT
issue "$host_id" agent.ext

openssl verify -x509_strict -CAfile ca.crt -purpose sslserver server.crt web.crt
openssl verify -CAfile ca.crt -purpose sslclient "$host_id.crt"
openssl x509 -in "$host_id.crt" -noout -ext subjectAltName
```

每台 Agent 使用独立私钥和证书，重新设置 `host_id` 并签发。证书中须恰好包含一个 `urn:monitor:host:<host-id>` 身份 URI，并与配置的 `host-id` 完全一致。将 CA 私钥留在签发机。

| 安装位置 | 证书及对应 JSON 字段 |
| --- | --- |
| Server | `server.crt` → `tls-cert`，`server.key` → `tls-key`，签发 Agent 的 `ca.crt` → `tls-ca` |
| Agent | `linux-node-01.crt` → `tls-cert`，对应私钥 → `tls-key`，签发 Server 的 `ca.crt` → `tls-ca` |
| Web | `web.crt` → `web-cert`，`web.key` → `web-key`，浏览器信任签发 Web 证书的 CA |

## 安装 Linux systemd 服务

从对应解压包目录运行命令。Server 使用专用系统用户。Agent 以 root 运行，便于读取进程指标及硬件清单。

Server 主机上执行以下命令。系统中已有同名账号时沿用该账号。

```sh
sudo groupadd --system monitor-server
sudo useradd --system --gid monitor-server --no-create-home --shell /usr/sbin/nologin monitor-server
sudo install -d -m 0755 /opt/monitor/bin /etc/monitor
sudo install -m 0755 bin/monitor-server /opt/monitor/bin/monitor-server
sudo install -m 0644 configs/server-postgres.json /etc/monitor/server.json
sudo install -m 0644 monitor-server.service /etc/systemd/system/monitor-server.service
sudo install -m 0600 /dev/null /etc/monitor/server.env
```

用 `sudoedit /etc/monitor/server.env` 设置凭据。替换以下示例值，并将相应的 CA 文件放在 Server 上。DSN 中的 URI 特殊字符需要百分号编码，也可使用 PostgreSQL 键值格式。

```ini
MONITOR_WEB_PASSWORD=REPLACE_WITH_WEB_PASSWORD
MONITOR_DB="host=db.example.com port=5432 dbname=monitor user=monitor password=REPLACE_WITH_DB_PASSWORD sslmode=verify-full sslrootcert=/etc/monitor/db-ca.crt"
```

数据库管理员先创建数据库，并赋予应用账号目标 schema 的建表及读写权限。Server 会初始化表和索引。若使用 SQLite，复制 `server-sqlite.json`，把 `db` 改为 `/var/lib/monitor/monitor.db`，环境文件只填写 Web 密码。systemd 会创建和授权 `/var/lib/monitor`。

编辑 `/etc/monitor/server.json`，把证书相关字段改为 `/etc/monitor` 下的绝对路径。Server 私钥可设 `root:monitor-server` 和 `0640`，证书及 CA 用 `0644`。若开放外部 Web 访问，配置实际的 `web-listen` 和 Web 证书，防火墙按实际访问来源开放对应端口。

Agent 主机上执行以下命令。

```sh
sudo install -d -m 0755 /opt/monitor/bin /etc/monitor
sudo install -m 0755 bin/monitor-agent /opt/monitor/bin/monitor-agent
sudo install -m 0600 configs/agent.json /etc/monitor/agent.json
sudo install -m 0644 monitor-agent.service /etc/systemd/system/monitor-agent.service
```

编辑 `/etc/monitor/agent.json`，填写 Server 地址、主机 ID 和证书绝对路径。Agent 私钥用 `root:root` 和 `0600`。配置 `process-services` 后才采集进程明细。共享 Token 可放在两端 JSON 文件或服务各自的环境文件中。Server JSON 若包含秘密，改为 `root:monitor-server` 和 `0640`。

在各主机启用其对应服务。将命令中的 `monitor-server` 替换为 `monitor-agent` 可操作 Agent。

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now monitor-server.service
sudo systemctl status monitor-server.service
sudo journalctl -u monitor-server.service -n 50 --no-pager
```

修改 JSON 或环境文件后重启对应服务。修改 systemd 单元后先执行 `daemon-reload`，再重启。Web 的 `/healthz` 可检查 HTTP 服务，登录后可检查 Agent 在线状态及历史。

## 启动 Windows Server

解压 Windows AMD64 包。在 PowerShell 中进入解压目录，准备 Server JSON，并按上述方式放置证书。JSON 中使用正斜杠路径或对反斜杠进行转义。SQLite 的父目录须先创建。

```powershell
New-Item -ItemType Directory -Force .\data
$env:MONITOR_WEB_PASSWORD = Read-Host 'Web password'
.\bin\monitor-server.exe -config .\configs\server-sqlite.json
```

使用 PostgreSQL 时改用 `server-postgres.json`，通过当前进程环境 `MONITOR_DB` 提供实际 DSN。命令在前台启动，按 Ctrl+C 停止。

## 配置参数

JSON 键与命令行参数同名。优先级从低到高依次为默认值、配置文件、环境变量、显式命令行参数。相对路径以进程工作目录为基准。环境变量 `MONITOR_DB`、`MONITOR_WEB_PASSWORD`、`MONITOR_AUTH_TOKEN` 分别覆盖 `db`、`web-password`、`auth-token`。时长使用 `"1s"` 等字符串，布尔值使用 JSON 的 `true` 或 `false`。

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
| process-services | [] | 命名服务数组，最多 128 项；支持 systemd unit、cgroup、可执行文件匹配及 match_any |
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

### 进程服务选择器

`process-services` 的每项指定唯一 `name`，并配置 `match` 或 `match_any`。单项 `match` 可用 `systemd_unit`、`cgroup` 或绝对路径 `executable`。可执行文件匹配可用 `args_contains` 要求完整的 argv 项。需要归并不同启动方式时，使用 `match_any` 数组，每个元素配置 `executable` 和可选 `args_contains`。任一元素命中即归入该服务。`include_children` 默认为 true。Agent 包的 `configs/agent-services.example.json` 提供示例。

## 升级 PostgreSQL 历史索引

新 Server 会自动创建 `samples_by_host_order`。已有较大的 `samples` 表时，用应用 schema 的表所有者先执行包内的 `postgres-history-index.sql`，再启动新 Server。脚本使用 `CREATE INDEX CONCURRENTLY`，需在事务块之外执行。

```sh
psql -X -v ON_ERROR_STOP=1 -d monitor -f postgres-history-index.sql
```

数据库主机、账号及 TLS 参数按部署环境传给 psql。部署使用非默认 schema 时，先确认连接的 `search_path`。并发建索引被中断时，检查该索引的 `pg_index.indisvalid` 状态，清理无效索引后重新执行。
