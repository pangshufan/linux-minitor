# 安装与配置

Agent 提供 Linux AMD64/ARM64 和 Windows AMD64 包。Linux 内核要求 4.18 及以上。Windows 首版面向 Server 2022/2025，启动检查允许 Server 2019 及同代或更新的 Windows 客户端。Server 提供 Linux AMD64、Linux ARM64 和 Windows AMD64 包，内置 Web 页面，支持 SQLite 和 PostgreSQL。GitHub Actions 的 **Build packages** 工作流会在 push、PR 和手动触发时生成归档，产物可在对应运行的 **Artifacts** 中下载。Windows 包为 ZIP，Linux 包为 tar.gz。Linux 包中附带对应的 systemd 单元。

## 选择加密方式

| 链路 | 启用方式 | 关闭方式 |
| --- | --- | --- |
| Agent → Server gRPC | 两端设 `tls-enabled: true`，分别提供证书、私钥和受信 CA。Server 校验 Agent 证书中的主机身份 | 两端设 `tls-enabled: false`，无需 gRPC 证书 |
| 浏览器 → Server Web | Server 设 `web-tls-enabled: true`，同时配置 `web-cert` 和 `web-key` | 设 `web-tls-enabled: false` 使用 HTTP。默认回环监听在没有 Web 证书时也使用 HTTP |
| Server → PostgreSQL | DSN 设置 `sslmode=verify-full` 及 `sslrootcert`。数据库证书的 SAN 需匹配 DSN 的主机名 | DSN 显式设置 `sslmode=disable`，适用于受控测试网络 |

三个开关各自生效。发布包的推荐示例配置同时启用了网页签发；若改用明文 gRPC，除两端设置 `tls-enabled: false` 外，还应把 Server 的 `agent-issuer-cert`、`agent-issuer-key`、`agent-server-ca` 三项设为空以关闭签发。仅使用 HTTP 时也不能启用网页签发。HTTP 下登录凭据和会话无法获得链路加密保护。Agent 共享 `auth-token` 可在两端同时配置，明文 gRPC 下 Token 同样可被链路上的第三方读取。关闭 mTLS 后，Server 使用 Agent 声明的 `host-id`。

## 登录与资源 Dashboard

登录页和管理页面均提供「页面主题」选择，支持浅色、深色和跟随系统。偏好保存在当前浏览器，刷新、跨页面访问和退出登录后仍保留；跟随系统时会响应系统外观变化。图表、表格和输入控件随主题同步切换。

Web 使用独立登录页，账号与密码沿用 `web-user`、`web-password`（或 `MONITOR_WEB_PASSWORD`）。Cookie 登录会话有效期为 8 小时；退出后会话立即失效，Server 重启后需重新登录。HTTPS 下会话 Cookie 标记 Secure，并始终使用 HttpOnly、SameSite=Strict；修改类接口校验跨站请求。原有脚本仍可用 HTTP Basic Auth 调用 API，浏览器不再弹出 Basic Auth 登录框。

主机详情提供系统、服务和选定进程的 Dashboard。在线时页面每 5 秒通过现有 gRPC 会话查询最新快照；这不会改变 Agent 的采样/计算窗口，也不会额外写入历史样本。历史数据按 `push-interval` 上报，每 30 秒增量读取一次；页面合并当前浏览期间的短期快照与历史点，切换窗口或时间范围后重新查询对应历史。页面标明采样窗口、上报周期和采集时间；缺失值及断线期间不补零，也不插值补历史。

Linux 磁盘 Dashboard 同时展示读写吞吐、IOPS、`util_percent`（util%）、`avg_queue_depth`、`read_await_ms` 和 `write_await_ms`。需升级 Agent 才会提供新增指标。多队列 SSD / RAID 的 util% 不能单独判断饱和，应结合队列、延迟和吞吐。Windows Agent 尚未采集的磁盘指标显示未提供，不伪造为零。服务 CPU 使用单核 100% 的口径，多核服务可能超过 100%；进程按 PID 与启动标识追踪，重启后是新的进程对象。

## 首次部署：配套生成 PKI 并启用网页签发（推荐）

发布包中的 `configs/server-sqlite.json`、`configs/server-postgres.json` 默认配套启用 **gRPC mTLS、Web HTTPS 和网页签发**。从 Server 程序包根目录执行（Linux 或 Windows 的 Git Bash，要求 OpenSSL 1.1.1+）：

```sh
bash init-server-pki.sh
```

按提示输入 Agent 和浏览器实际访问的 Server IPv4 地址，以及可选的域名。脚本在新的 `config/` 目录生成整套证书，拒绝覆盖已有目录。仓库源码中对应命令是 `bash deploy/init-server-pki.sh`。Windows 没有 Git Bash/OpenSSL 时，可在签发机执行脚本后，将运行所需文件复制到 Windows Server；私钥仅交给负责部署的管理员。

| 生成文件 | Server 示例配置 | 用途 |
| --- | --- | --- |
| `config/server.crt` / `server.key` | `tls-cert` / `tls-key` | gRPC Server 身份，SAN 包含输入的地址 |
| `config/agent-ca.crt` | `tls-ca`、`agent-issuer-cert` | Server 验证 Agent；也是网页签发 CA |
| `config/agent-ca.key` | `agent-issuer-key` | 网页签发所需的专用私钥 |
| `config/server-ca.crt` | `agent-server-ca` | 放入 Agent ZIP，供 Agent 验证 gRPC Server |
| `config/web.crt` / `web.key` | `web-cert` / `web-key` | Web HTTPS 身份 |
| `config/server-ca.key` | 不配置到运行服务 | 离线保存，用于日后更新 Server/Web 证书 |

设置 Web 密码和数据库参数，创建 SQLite 数据目录 `data/`，然后从程序包根目录使用相应 JSON 启动。Web 默认地址是 `https://127.0.0.1:8080`；远程管理时将 `web-listen` 改为实际监听地址，并使用脚本中输入的 IP/域名访问。浏览器信任 `server-ca.crt` 后验证 Web 证书。

启动后进入 **Agent 证书** 页面，签发新主机并下载 ZIP；目标主机使用 Agent 程序包中的安装向导导入 ZIP。**新增 Agent 无需修改 Server 的 CA 文件或重启 Server**：网页签发使用的 `agent-ca.crt` 已在 Server 的 `tls-ca` 中，下载包也已包含正确的 `server-ca.crt`（包内命名为 `ca.crt`）。

新增主机时不要重新生成 CA。服务端证书换发但签发 CA 不变时，Agent 原来的 CA 可继续使用；只有更换信任根时才需要有计划地分发新的 CA 信任，并在过渡期保留原信任链。不要直接重新运行初始化来更新已有部署。

### 已有部署：启用网页签发


此节只适用于已有证书的部署，不要替换已有 Server/Agent 的 CA。程序在未配置签发 CA 时不会自行生成 CA，页面会禁用签发并显示缺少的配置。需要在 Monitor Server 启用 gRPC mTLS 和自身的 Web HTTPS，并配置签发 CA。能登录 Web 管理页的账号可以为任意主机 ID 签发证书，应仅把此账号交给有签发权限的管理员。

建议使用**专用于 Agent 签发的 CA**。已有 CA 允许签发中间 CA 时，可以配置专用的中间 CA 及其链。若已有 CA 使用了 `pathlen:0`，则不能签发中间 CA；可以生成一张独立的 Agent CA，将其公钥证书追加到 Server 的 Agent 信任列表，同时保留原 CA。这样现有 Server 证书及现有 Agent 均可继续使用，原 CA 私钥也可留在签发机。

以 Linux 为例，以下命令在签发机准备文件。先把 **Server 当前 `tls-ca` 文件**复制为 `existing-agent-trust.pem`，把**能够验证当前 gRPC Server 的 CA 公钥文件**复制为 `server-ca.crt`。两者可能相同，也可能不同，不能用 Web 专用 CA 代替。

```sh
umask 077
openssl req -x509 -newkey rsa:3072 -nodes -sha256 -days 1825 \
  -subj '/CN=Monitor Agent Issuing CA' -keyout agent-issuer.key -out agent-issuer.crt \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'extendedKeyUsage=clientAuth' \
  -addext 'subjectKeyIdentifier=hash'
cat existing-agent-trust.pem agent-issuer.crt > agent-client-ca-bundle.crt
```

把上述文件放到 Server，私钥只允许 Server 服务账号读取（例如 `root:monitor-server`、`0640`）。保留现有 `tls-cert`、`tls-key`、`web-cert` 和 `web-key`，在 `server.json` 中合入以下配置后重启一次 Server：

```json
{
  "tls-enabled": true,
  "web-tls-enabled": true,
  "tls-ca": "/etc/monitor/agent-client-ca-bundle.crt",
  "agent-issuer-cert": "/etc/monitor/agent-issuer.crt",
  "agent-issuer-key": "/etc/monitor/agent-issuer.key",
  "agent-server-ca": "/etc/monitor/server-ca.crt"
}
```

Server 启动会验证签发 CA 受 `tls-ca` 信任，并验证 `agent-server-ca` 能校验当前 gRPC Server 证书；两者不配套会拒绝启动并提示修正，不应等 Agent 连接后再排查。修改 `tls-ca` 文件后必须重启 Server，磁盘内容变化不会自动改变已加载的信任列表。

此后在 Web 点击 **Agent 证书**，填写唯一主机 ID、目标系统、Agent 能访问的 gRPC 地址（例如 `10.181.21.20:8443`）及有效天数，再点击「签发并下载」。有效期超过 365 天时仅提示，不强制限制；到期时间仍不能晚于签发 CA 及其验证链的到期时间。地址须匹配当前 gRPC Server 证书的 SAN。每次签发不需要重启 Server。

ZIP 中有 `agent.json`、`agent.crt`、`agent.key` 和用于验证 gRPC Server 的 `ca.crt`。解压至目标机的 `/etc/monitor`（Linux）或 `C:\ProgramData\Monitor`（Windows）；若目录不同，更新 `agent.json` 中的绝对路径。配置了共享 Token 的环境还需在目标 Agent 补填；ZIP 不包含 Token。为现有 Agent 续期时，将新证书和新私钥一起替换，保留原有采集配置，然后重启 Agent。

请将 ZIP 作为私钥交付，只交给目标主机。Server 不存储生成的 Agent 私钥，不支持找回旧下载或吊销单张证书；再次签发不会让旧证书失效。Server 日志记录签发账号、主机 ID、证书序列号和到期时间。

「Agent 证书」页同时显示当前 Server 的 gRPC、Web HTTPS 和签发 CA 证书的到期时间及剩余天数（只显示已配置的证书）。主机详情显示 Server 最近一次验证通过的 Agent 客户端证书到期时间；离线时标注为上次连接证书，重连后更新。旧主机在升级后首次重连前没有该信息，明文连接不显示证书。修改磁盘上的 Server 证书后须重启 Server 才会加载。

## 按提示安装 Agent（推荐）

从发布产物下载与目标系统、架构对应的 **Agent 程序包**，完整解压。网页签发的证书 ZIP 是连接配置包，不含 Agent 可执行文件；保留这个 ZIP，无需手动解压或修改其中的路径。

Linux（内核 4.18+，systemd，须具备 Python 3）在程序包根目录执行：

```sh
sudo bash install-agent.sh
```

Windows Server 2019+ x64：以管理员身份打开 CMD，进入程序包根目录执行：

```cmd
install-agent.bat
```

也可在管理员 PowerShell 中执行 `powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\install-agent.ps1`。脚本依次询问：

1. 网页下载的证书 ZIP 路径、解压后的证书目录或已有 `agent.json` 路径。若暂不使用证书，可以留空后手动填写连接信息。
2. Server gRPC 地址、唯一主机 ID、是否启用 mTLS。导入证书包后会提供默认值；主机 ID 必须与签发时一致。
3. 证书路径（默认直接使用导入包内的文件）、是否设置共享 Token。关闭 mTLS 的环境不要求证书。
4. Windows 额外询问是否启用 `tlsmlkem=0`。仅在该设置已验证能解决握手问题时选择 y；设置只写入 MonitorAgent 服务环境。
5. 确认安装或更新并启动服务。

Linux 默认把二进制放到 `/opt/monitor/bin`，配置和证书放到 `/etc/monitor`，注册 `monitor-agent.service`。Windows 默认把二进制放到 Program Files 下的 Monitor 目录，配置和证书放到 ProgramData 下的 Monitor 目录，注册 `MonitorAgent`。脚本会重写配置中的证书绝对路径，并限制私钥访问权限。导入已有配置时保留进程监控等设置；若导入全新证书包替换已有安装，应先把原有采集设置合入新配置。

更新会先备份已有配置、证书、二进制及服务配置，再停止服务并替换文件。备份位于配置目录下的 `backup-日期时间-进程号`。安装失败时根据终端错误修复，必要时停止服务并从备份恢复；脚本不自动回滚。服务进程运行不代表已成功注册，最后仍须在 Web 服务器列表确认主机在线。

以下章节保留手工安装步骤，适用于自定义部署目录和服务策略。

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

如果采用首次部署脚本，在程序包根目录把运行所需证书复制到 `/etc/monitor`（不要复制 `server-ca.key`）：

```sh
sudo install -o root -g monitor-server -m 0644 config/server.crt config/web.crt config/server-ca.crt config/agent-ca.crt /etc/monitor/
sudo install -o root -g monitor-server -m 0640 config/server.key config/web.key config/agent-ca.key /etc/monitor/
```

编辑 `/etc/monitor/server.json`，将以下字段作为一组配置，不能只替换 `tls-cert`/`tls-key` 而漏掉其余路径：

```json
{
  "tls-enabled": true,
  "tls-cert": "/etc/monitor/server.crt",
  "tls-key": "/etc/monitor/server.key",
  "tls-ca": "/etc/monitor/agent-ca.crt",
  "agent-issuer-cert": "/etc/monitor/agent-ca.crt",
  "agent-issuer-key": "/etc/monitor/agent-ca.key",
  "agent-server-ca": "/etc/monitor/server-ca.crt",
  "web-tls-enabled": true,
  "web-cert": "/etc/monitor/web.crt",
  "web-key": "/etc/monitor/web.key"
}
```

这是要合入原 JSON 的片段；保留数据库、密码、监听地址等其他配置。若开放外部 Web 访问，配置实际的 `web-listen`，防火墙按实际访问来源开放对应端口。已有部署采用前一节的过渡信任列表，不要用新 Agent CA 覆盖原有信任。

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

## 安装 Windows Agent

解压 Windows AMD64 Agent 包。在管理员 PowerShell 中准备配置目录和证书，先以控制台方式确认可以连到 Server。

```powershell
$dir = Join-Path $env:ProgramData Monitor
New-Item -ItemType Directory -Force $dir
icacls $dir /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F'
Copy-Item .\configs\agent-windows.json (Join-Path $dir agent.json)
notepad (Join-Path $dir agent.json)
.\bin\monitor-agent.exe -config (Join-Path $dir agent.json)
```

按实际环境填写 Server 地址、与证书 URI 匹配的主机 ID、证书及私钥路径。Windows Service 的工作目录由系统管理，配置中的证书应使用绝对路径。服务安装脚本把可执行文件复制到 Program Files，注册固定名称 `MonitorAgent`，以 LocalSystem 服务账号自动启动。控制台验证完成后按 Ctrl+C，然后运行：

```powershell
.\install-agent.ps1
Get-Service MonitorAgent
Restart-Service MonitorAgent
```

只需卸载服务时，在管理员 PowerShell 中执行 `Stop-Service MonitorAgent`，再执行 `sc.exe delete MonitorAgent`。升级时先停止服务，替换 Program Files 下 Monitor 目录中的可执行文件，再启动服务。

Windows 默认不采集进程明细。在 `process-services` 中填写 SCM 的服务名称，或选择可执行文件的绝对路径。可通过 `Get-Service` 的 Name 字段查询 SCM 名称。同路径的多个进程可归入同组。若需区分使用同一个 Java 可执行文件的多个服务，使用各自的 SCM 服务名。

```json
[
  {"name":"Print Spooler","match":{"windows_service":"Spooler"}},
  {"name":"应用进程","match":{"executable":"C:/Apps/example/example.exe"}}
]
```

Windows 首版采集整机 CPU、物理内存、启动时间、网卡速率、本地固定卷容量、基础硬件清单、已配置服务状态，以及可访问进程的 CPU、工作集和句柄数。支持进程树和多计算窗口。共享宿主进程的服务只报告 SCM 状态，权限不足或服务处于启停过渡状态时报告不完整或未知。Windows 暂无命令行参数选择器、PSS、磁盘吞吐率、物理磁盘拓扑和 Storage Spaces 拓扑。

### 排查 Windows Agent 的 TLS 握手失败

如果 Agent 报 `tls: failed to verify certificate: x509: certificate signed by unknown authority`，先确认 `tls-ca` 指向签发 ZIP 中的 `ca.crt`，而不是 Web HTTPS 的 CA。用 `certutil -hashfile <ca.crt 的实际路径> SHA256` 比对下载包与 Agent 实际读取的文件。从 `bin` 启动时，证书的相对路径以 `bin` 为基准；也可在配置中直接填写绝对路径。

如果 CA 一致，比较同一台 Windows 上 Go 客户端和 OpenSSL 收到的服务端证书指纹。可用下面的命令读取 OpenSSL 收到的证书，将地址换成配置中的 gRPC 地址。在 Git Bash 执行：

```sh
openssl s_client -connect monitor.example.com:8443 -alpn h2 -showcerts </dev/null |
  openssl x509 -noout -issuer -subject -fingerprint -sha256
```

这条命令只用于查看证书。没有提供 Agent 客户端证书时，Server 返回 `certificate required` 属于预期行为；拿到服务端证书不代表 mTLS 已通过。

在测试中，OpenSSL 收到了 Monitor Server 的证书，但 Go 客户端收到了不同的证书。临时关闭 Go 默认提供的混合后量子密钥交换后，Agent 恢复连接。如果出现同样现象，可以在 Agent 的 `bin` 目录用 CMD 验证：

```cmd
cmd /d /c "set GODEBUG=tlsmlkem=0&& monitor-agent.exe -config ..\configs\agent-windows.json"
```

这个[兼容性设置](https://go.dev/src/crypto/tls/common.go)仅对该次启动生效，CA、主机名和客户端证书校验仍然启用。不需要换证书，也不要信任握手中收到的陌生 CA。若使用 Windows 服务，交互式 CMD 的变量不会传给 `MonitorAgent`，需要在服务的运行环境中单独配置。若设置后错误仍存在，继续核对该连接收到的服务端证书，无需把此设置留作默认值。

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
| agent-issuer-cert / agent-issuer-key | 空 | 选填，网页签发的 CA 证书链和私钥；启用时需 gRPC mTLS 与 Web HTTPS，且 CA 须受 `tls-ca` 信任 |
| agent-server-ca | 空，签发启用时回退到 `tls-ca` | 签发 ZIP 中分发的 CA 公钥证书，用于 Agent 验证 gRPC Server；双向使用不同 CA 时须显式配置 |
| auth-token | 空 | 可选共享 Agent Token，设置后强制校验 |
| web-tls-enabled | true | false 强制 HTTP；true 在提供证书时使用 HTTPS，非回环监听必须提供证书；回环地址无证书保留 HTTP 行为 |
| web-cert / web-key | 空 | Web HTTPS 证书和私钥，启用时成对配置 |
| web-user | monitor | Web/API 登录账号 |
| web-password | 空，必填 | Web/API 密码，也可通过 MONITOR_WEB_PASSWORD 提供 |
| alerts | 总开关关闭 | 告警规则、默认阈值及 Webhook 配置见 [告警配置与对接](alerts.md) |
| offline-after | 15s | 心跳超时，必须大于 1s |
| retention | 168h | 历史数据保留时间，必须为正，每小时清理样本、无引用的配置版本和符合条件的已关闭告警 |

### 进程服务选择器

`process-services` 的每项指定唯一 `name`，并配置 `match` 或 `match_any`。单项 `match` 可用 `systemd_unit`、`cgroup` 或绝对路径 `executable`。可执行文件匹配可用 `args_contains` 要求完整的 argv 项。需要归并不同启动方式时，使用 `match_any` 数组，每个元素配置 `executable` 和可选 `args_contains`。任一元素命中即归入该服务。`include_children` 默认为 true。Agent 包的 `configs/agent-services.example.json` 提供示例。

## 升级 PostgreSQL 历史索引

新 Server 会自动创建 `samples_by_host_order`。已有较大的 `samples` 表时，用应用 schema 的表所有者先执行包内的 `postgres-history-index.sql`，再启动新 Server。脚本使用 `CREATE INDEX CONCURRENTLY`，需在事务块之外执行。

```sh
psql -X -v ON_ERROR_STOP=1 -d monitor -f postgres-history-index.sql
```

数据库主机、账号及 TLS 参数按部署环境传给 psql。部署使用非默认 schema 时，先确认连接的 `search_path`。并发建索引被中断时，检查该索引的 `pg_index.indisvalid` 状态，清理无效索引后重新执行。

## 离线手工签发（高级部署）

这是不使用网页签发的独立部署方式。不要与上面的首次部署步骤混用。Server 配置中将 `agent-issuer-cert`、`agent-issuer-key` 和 `agent-server-ca` 设为空，把 `tls-ca` 设为这里的 `ca.crt`；Agent 也使用这里的 `ca.crt` 验证 Server。其他证书路径按下表配置。启用网页签发时再按已有部署迁移步骤添加专用 CA。

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
