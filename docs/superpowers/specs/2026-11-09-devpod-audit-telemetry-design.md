# DevPod 审计与遥测设计（Audit & Telemetry）

**Status:** Draft — 待作者审阅
**Date:** 2026-11-09
**Audience:** project author and future implementers

为 DevPod 增加两类能力，并汇总到平台（webui）上：

1. **会话审计**：在 SSH gateway 上记录会话、exec 命令、TTY 录像，以及
   scp / sftp / lrzsz 传输的文件内容；提供一组可选的硬策略。
2. **容器遥测**：由容器内的 supervisor 采集 1 秒粒度的 CPU / 内存 /
   磁盘 IO / 网络 / RDMA / GPU 指标、按进程的资源用量、容器内部信息，
   以及把进程归属到具体 SSH 会话（用户）。

同时重构 webui 的 DevPod 列表页（高密度表格）与详情页。

本文档中「gateway」指 `cmd/gateway`，「supervisor」指 `cmd/supervisor`，
「webui」指 `cmd/webui` + `web/`。字段名、API 路径、表名使用英文标识。

---

## 1. 目标与非目标

### 目标

- 审计服务三类用途：**合规 / 防外泄**、**安全事件追溯**、**运维排障 /
  行为可见**。
- gateway 是可信的审计数据来源：会话元数据、exec 命令、session channel
  的双向原始流（TTY、exec、sftp/scp 子系统）全量记录；在原始流上解析
  SFTP、legacy scp、ZMODEM，得到文件事件并保存文件原文。
- 端口转发默认只记元数据，策略可开启原始流录制或禁止转发。
- 可选硬策略（默认全部 allow）：禁止上传 / 下载、单文件大小上限、禁止
  端口 / agent / X11 转发、fail-open / fail-closed。
- 容器侧补充命令语义（shell 钩子）和进程级视图，并把进程归属到 gateway
  会话；容器侧数据标记为「参考级」。
- 1 秒粒度的资源指标与按进程资源用量，实时显示在 webui；RDMA 与内核
  协议栈网络分开展示。
- 权限：管理员可看全部；普通用户可看自己发起的会话（含录像），以及自己
  作为 owner 的 DevPod 的监控。查看录像 / 下载原文等操作写入访问记录。
- DevPod 列表页改为高密度表格，详情页增加监控 / 进程 / 会话 / 容器信息
  标签页。

### 非目标

- **防篡改**：假设 S3 与 ClickHouse 可信。不做段哈希链、Object Lock、
  WORM。
- **内容级 DLP**：不做关键词 / 文件类型匹配阻断、不做审批流。
- **拦截不透明传输**：`cat` / `base64` 复制粘贴、exec 内的 `tar` /
  `rsync`、经 VS Code 转发通道的下载、不经 SSH 的出站流量——只记录，
  不拦截。
- **计费级指标**：容器侧指标仅用于展示和排障；配额仍按 spec 声明的
  limits 计算。
- 会话实时旁观 / 管理员强制断开会话。
- 按 DevPodTemplate 配置审计策略（仅支持集群默认 + 按用户覆盖）。
- 审计员（auditor）独立角色。

---

## 2. 前置依赖与环境假设

### 2.1 前置依赖（单独立项，不在本 spec 范围内）

本设计的「防外泄」目标依赖以下问题先得到解决；否则策略只能拦住
不打算绕过的用户：

1. **webui 允许非管理员创建特权 Pod**：`internal/webui/api_devpods.go`
   的 YAML 路径原样透传 PodSpec，而 `devpods` 命名空间的 PodSecurity
   级别为 `privileged`。
2. **NetworkPolicy 从未生效**：controller 不读取 `GatewayConfig` CR
   （`cmd/controller/main.go` 用启动参数构造配置），`isolateNetwork`
   恒为 false，容器出站不受控。
3. **`kubectl exec` 不经过 gateway**。本设计只能检测（`untracked_entry`），
   不能阻止。

### 2.2 环境假设（写入运维文档）

- gateway 到 DevPod Pod IP 之间**没有 SNAT**（常见 CNI 满足）。
- `devpods` 命名空间**不注入 Istio 等 sidecar**（否则入站对端地址变为
  `127.0.0.6`，会话关联失效）。
- 集群节点做了 **NTP 时间同步**。
- 节点优先使用 **cgroup v2**；supervisor 兼容 v1。三期 node-agent 要求
  v2。

---

## 3. 总体架构

```
 client ──SSH──► gateway ───────────── SSH（内部密钥）─────────────► DevPod 容器
                  │ Proxy ──旁路采集──► internal/audit                 sshd（supervisor 拉起）
                  │                    ├ Session（每会话一个）         ├ shell 钩子 → /run/devpod/hooks/
                  │                    ├ Decoders: exec/sftp/scp/zmodem│
                  │                    ├ Policy（可选，在线执行）       supervisor（PID 1）
                  │                    └ Spool（本地盘）─► Uploader     └ telemetry 子进程
                  │                                 ├─► S3（原始流、文件原文）   ├ /proc 采样 + 进程归属
                  │                                 └─► ClickHouse（审计事件）   ├ cgroup/网络/RDMA/GPU 指标
                  │                                                            └ 环形缓冲 ─┐
 webui ◄── :8081 /ingest（集群内）◄──────────── HTTP 批量推送（每秒）──────────────────────┘
   │  ├─► ClickHouse（容器侧数据、告警、访问记录）
   │  ├─ leader：交叉校验、归属回填
   │  └─ 读 ClickHouse + 代理读取 S3 ──► 页面：DevPod 列表/详情、审计、管理
```

### 3.1 职责划分

| 组件 | 职责 |
|---|---|
| gateway | SSH 层审计（可信）：采集、解析、策略执行、写 S3 与 ClickHouse。**不**维护到容器的长连接，**不**向容器注入任何信息，只在会话记录里写下后端连接的本地地址 `backend_local_addr` |
| supervisor | 容器内遥测（参考级）：shell 钩子、进程采样与归属、资源指标、容器信息，推送到 webui |
| webui | 平台：接收容器数据（`/ingest`）、交叉校验、归属回填、查询与展示、策略编辑 |
| controller | 把 ingest 令牌和遥测配置写进 per-DevPod host-key Secret；渲染出站 NetworkPolicy 规则 |
| node-agent（三期） | 特权 DaemonSet：Tetragon 事件转发、NVML 按进程 GPU、cgroup_skb 精确网络计量 |

### 3.2 部署模式（chart）

| values | 行为 |
|---|---|
| `audit.enabled=false`（默认） | gateway 行为与现在完全一致 |
| `audit.s3.bundled.enabled=false`（默认） | 使用外部 S3：`endpoint`、`bucket`、`region`、凭据 Secret |
| `audit.s3.bundled.enabled=true` | 部署单副本 **Silo**（`docker.io/pgsty/silo`，MinIO 社区 fork）StatefulSet + PVC；bucket 与生命周期规则由 migrate Job（`devpod-webui migrate`，aws-sdk）创建。镜像可通过 values 替换（例如 MinIO AIStor Free） |
| `audit.clickhouse.bundled.enabled=false`（默认） | 使用外部 ClickHouse |
| `audit.clickhouse.bundled.enabled=true` | 用官方 `clickhouse/clickhouse-server` 镜像部署单副本 StatefulSet + PVC |

- 模板自行编写，不依赖第三方 subchart。
- 自带模式面向小规模 / 试用，无高可用。
- 自带组件配 NetworkPolicy：只允许 gateway、webui、建表 Job 访问。
- 建表由 Helm hook Job 执行（DDL 账号，迁移脚本带版本号，记录在
  `schema_migrations` 表）。每次 `helm upgrade` 时该 Job 还会按当前
  `audit.retentionDays` 重新应用 ClickHouse 表 TTL（`ALTER TABLE ...
  MODIFY TTL`），并在 `audit.s3.manageLifecycle=true` 时设置 S3 bucket
  的生命周期规则（自带 Silo 默认 true；外部 S3 默认 false，由管理员自行
  配置）。

### 3.3 分期

| 期 | 内容 |
|---|---|
| **一期** | gateway 审计（§4）、策略（§7）、存储（§8）、平台的审计部分与列表 / 详情页新布局（§9）、chart 部署模式（§3.2） |
| **二期** | supervisor 遥测（§5、§6）：shell 钩子、轮询方式的进程归属、1 秒指标（含 RDMA、GPU）、容器信息、`/ingest`、交叉校验；列表 / 详情页的监控与进程数据 |
| **三期** | node-agent（§5.6、§6.6）：Tetragon 进程事件、按进程精确 GPU、cgroup_skb 精确网络（含 hostNetwork UDP） |

各期验收标准见 §13。实现计划按期分别编写，先写一期。

---

## 4. gateway：录制与解析（一期）

代码位置：`internal/audit`（新包）、`internal/audit/rawlog`（gateway 与
webui 共用的原始流编解码），`internal/gateway/proxy.go` 增加采集点。

### 4.1 会话与 channel 模型

- **会话**：一条客户端 SSH 连接。接受连接时生成 **ULID** 作为会话 ID，
  替换现有的副本内递增 `sid-<n>`。会话 ID 贯穿 spool 路径、S3 key、
  ClickHouse 行。
- **channel**：会话内的 channel，带会话内序号、类型（`session` /
  `direct-tcpip` / `forwarded-tcpip` / `auth-agent@openssh.com` /
  `x11`）和解析后的附加信息（如 direct-tcpip 的目标地址与端口）。
- gateway 拨号后端成功后，把后端 TCP 连接的本地地址
  `backend_local_addr`（gateway Pod IP:临时端口）记入会话（§5.4 用于关联）。

### 4.2 采集点

将 `proxy.go` 中的 `countingReader` 替换为通用旁路采集：每个 channel 的
双向 request（含应答结果）、stdin / stdout / stderr 数据、channel 关闭，
同步交给 `audit.Session`。采集点只做两件事：追加写本地 spool、交给解析器
与策略引擎；不做阻塞 I/O 以外的操作。审计关闭时采集点为空操作。

### 4.3 原始流格式（真相源）

每会话一条只追加写的记录流，单条记录：

```
[时间戳 ns varint][channel 序号 varint][类型 byte][长度 varint][载荷]
类型：channel_open | request | data_in | data_out | data_err | channel_close | gap
```

- 每满 **8 MiB 或 60 秒**切一个分段，zstd 压缩后上传为
  `raw/<yyyy>/<mm>/<dd>/<session>/<seq>.seg.zst`；会话结束时立即切段上传。
- **端口转发 channel 默认只记 `channel_open` / `channel_close`（含双向
  字节数），不记数据记录**；策略 `forwarding.recordContent=true` 时记录
  数据。
- 录像、文件事件等均可由原始流重新推导；解析器改进后可对历史数据重跑。

### 4.4 解析器

| 解析器 | 触发条件 | 产出 |
|---|---|---|
| 会话 request | `pty-req` / `shell` / `exec` / `subsystem` / `window-change` / `env` / `exit-status` / `exit-signal` | channel 元数据；exec 命令事件（命令行、退出码、耗时） |
| SFTP | `subsystem sftp`（含 OpenSSH ≥ 9 的 `scp`） | 按包解析 SFTP v3（双向）。以 handle 跟踪打开的文件，按 offset 重组 READ 响应 / WRITE 请求数据；CLOSE 时产出文件事件（方向、路径、大小、sha256、是否完整）并上传原文。REMOVE / RENAME / MKDIR / RMDIR / SETSTAT 产出元数据事件 |
| legacy scp | `exec scp -t ...`（上传）/ `exec scp -f ...`（下载），含 `-r` | 解析 `C` / `D` / `E` / `T` 控制行与数据块，产出文件事件与原文 |
| ZMODEM | pty 输出中检测到 `**\x18B00`（`sz`，下载）或 ZRINIT（`rz`，上传） | 解析 ZFILE 头（文件名、大小）、ZDLE 转义、ZDATA 数据块与 CRC，重组原文；用 CRC + 大小判定是否完整 |
| 不透明传输 | exec 命令匹配 `rsync --server`、`tar`、`cat >` 等 | 「疑似传输」事件；内容在原始流中 |
| 转发 / 其他 | direct-tcpip、forwarded-tcpip、agent、X11 | 元数据事件：目标、双向字节数、时长 |

- 文件原文写入 spool 临时文件（随机写按 offset 落位），完成后上传为
  `files/<yyyy>/<mm>/<dd>/<session>/<channel>-<n>`。**不做跨会话去重**；
  sha256 记入 `file_events` 用于检索与比对。
- SFTP「是否完整」：已覆盖的字节区间是否覆盖 `[0, size)`。
- ZMODEM 解析器最复杂且无成熟 Go 库，需自研；第一版目标为检测 + 文件名 /
  大小 + 尽力重组原文 + 完整性判定。

### 4.5 录像

asciicast v2（输出 `o`、输入 `i`、尺寸变化 `r`）**不在 gateway 生成**，
由 webui 在回放时从原始流现场转换（`internal/audit/rawlog`）。输入事件
`i` 包含关闭回显时键入的内容（如 sudo 密码），靠访问控制保护（§9.2）。

### 4.6 故障隔离

每个 channel 的解析器包一层 `recover`。panic 后停止该 channel 的解析，
记 `decoder_error` 事件；原始流采集照常，会话不受影响。策略侧处理见 §7.4。

### 4.7 登录提示

审计开启时，gateway 在现有 `GatewayConfig.spec.banner` 之前追加一行录制
提示（文案可配置，默认「本会话将被记录用于安全审计」）。

### 4.8 gateway 的其他改动

- 从 informer 缓存按会话读取 `GatewayConfig`（已有 informer），替代
  启动时一次性读取的 `loadGatewayConfig`，使策略与审计开关无需重启生效。
- 新增指标：`audit_spool_bytes`、`audit_upload_lag_seconds`、
  `audit_gaps_total`、`audit_decoder_errors_total`、
  `audit_policy_actions_total{rule,action}`。

---

## 5. 容器侧：会话归属与上报（二期，node-agent 部分为三期）

### 5.1 shell 钩子

- supervisor 在 `/etc/profile.d/` 安装钩子脚本（沿用现有
  `installProfileScripts`）：bash 用 DEBUG trap + `PROMPT_COMMAND`，
  zsh 用 `add-zsh-hook preexec/precmd`，fish 用
  `fish_preexec/fish_postexec`。读取 profile.d 的交互式 shell（自带或
  用户镜像中的）均生效。
- 钩子用 shell **内建 `printf`** 追加写 `/run/devpod/hooks/<pid>`
  （`/run/devpod` 不可写时回退到 `/tmp/.devpod/hooks/`），**每条命令
  不 fork**。记录字段：shell 的 `$$` 与 starttime、tty、cwd、命令原文、
  开始 / 结束时间、退出码。字段以 `\x1f` 分隔、记录以 `\0` 结尾。
- supervisor 用 inotify 监听目录，解析为 `command` 事件
  （`source=shell_hook`）。读取后截断文件；对应 shell 退出后删除文件；
  单个文件超过 1 MiB 时截断并记 `gap`。
- 钩子能看到进程追踪看不到的内容：内建命令（`cd`、`echo > 文件`）、
  别名、命令原文、cwd、退出码。**两种进程事件来源下都保留钩子**。
- 局限：`sh` / `dash`、脚本内命令、`ssh host cmd`（exec，由 gateway
  记录）不触发；用户可关闭钩子（由 `hook_missing` 检测）。

### 5.2 统一进程事件模型

所有来源归一为：`process_start`、`process_exit`、`ssh_conn`、
`untracked_entry`，每条带 `source = supervisor | tetragon`。归属、
交叉校验、展示只认统一模型。两个来源同时存在时，展示优先采用 tetragon，
supervisor 数据保留作对照。

### 5.3 进程归属规则（两种来源通用，按优先级）

进程一律以 `(pid, starttime)` 标识，避免 pid 复用误判。

1. **进程树**：祖先链上存在合法 `sshd-session` → 属于该 SSH 连接。
   合法 = 可执行文件为 `/opt/devpod/libexec/sshd-session`（只读挂载）
   **且**父进程是 supervisor 启动的 sshd。
2. **`SSH_CONNECTION`**：读 `/proc/<pid>/environ` 中 sshd 自身设置的
   `SSH_CONNECTION`（非注入），其客户端 IP:端口即 `backend_local_addr`。
   该变量随进程继承、守护化后仍在，覆盖 tmux / nohup / setsid 场景。
3. **会话 ID（SID）或控制终端**匹配已归属的进程。
4. **时间窗口推断**：该 DevPod 当时只有一个活跃会话时归给它，标注
   「推断」。
5. 以上都不满足 → `unattributed`。
- `PPid == 0` 且不是 1 号进程 → `untracked_entry`（经 `kubectl exec`
  等方式直接进入容器）。
- **粘性归属**：首次看到进程即记录归属；之后被过继给 1 号进程归属不变；
  其子进程继承已记录的归属。
- 归属粒度为 gateway 会话（一条 SSH 连接）；同一连接内多个 channel 的
  对应按启动时间尽力匹配，标注「推断」。
- 已知局限：tmux server 被另一会话 attach 后，仍归属最初创建它的会话。

### 5.4 连接与 gateway 会话的关联

- supervisor 对每个合法 `sshd-session` 上报
  `ssh_conn{pid, starttime, peer_addr}`：经 `/proc/<pid>/fd` 找到 socket
  inode，在 `/proc/<pid>/net/tcp{,6}` 中查对端地址（使用该进程的网络
  命名空间，hostNetwork 同样适用）。
- 关联键：`(devpod_uid, peer_addr = backend_local_addr)` + 时间区间重叠。
- webui 接收数据时尝试解析会话 ID；解析不到（gateway 的
  `session_open` 尚未入库）记为「待定」，由 leader 每分钟回填（重新插入
  新版本行，不用 `ALTER UPDATE`）。

### 5.5 supervisor 来源（二期）

- **遥测在独立子进程 `devpod-supervisor telemetry` 中运行**，由 1 号
  进程监管（崩溃后退避重启），设置 `GOMEMLIMIT`。1 号进程崩溃会重启整个
  容器，遥测 bug 不得连带用户。
- 每 **500ms** 发现新进程、每秒发现退出（§5.3 归属）。为把开销压到单核
  1% 以下（§6.8，实测见 `docs/superpowers/notes/2026-11-telemetry-phase2-measurements.md`）：
  - 每秒只读运行过的进程的 `/proc/<pid>/stat`，空闲进程每 10 秒读一次；
    exe / cmdline / environ / status 每个进程只读一次（exec 后重读）；
    `io` 只在 CPU 时间变化时读；速率按两次读取的实际间隔计算；
  - 两次完整扫描之间用 `/proc/loadavg` 的最新 pid 判断有无新进程，只探测
    新分配的 pid，不列 `/proc`；
  - 「socket → 进程」映射每 5 秒刷新，只遍历期间运行过的进程的 fd；
    SSH 连接的对端每个会话只解析一次；
  - 遍历全节点 TCP 表的读取（sock_diag 每进程 TCP 字节、`/proc/net/tcp`
    监听端口）按「不超过单核 0.1%」自适应间隔，大节点上约 18 秒一次，
    每进程 TCP 速率为该间隔内的平均；容器级网络仍为 1 秒；
  - 子进程 `GOMAXPROCS=2`。
- 开关 `processTracking`（见 §10.3）：只开 Tetragon 时可关闭轮询，钩子
  照常工作。

### 5.6 Tetragon 来源（三期）

- Tetragon 用官方 chart 自行安装；本项目 chart 提供可选的 TracingPolicy：
  **只启用 `process_exec` / `process_exit`，在内核中按 `devpods` 命名空间
  过滤；禁止文件、ioctl、网络包级探针**。
- node-agent（DaemonSet）从本机 Tetragon gRPC 读取事件，推送到
  `/ingest`，用 ServiceAccount 令牌认证（webui 调 TokenReview 校验）。
- Tetragon 事件与 `ssh_conn` 按 `(Pod, 容器, sshd-session, 启动时间
  ±10ms)` 关联（两侧启动时间都来自内核 `task->start_time`），实现时实测。
- Tetragon agent 运行在系统预留核上，不占用 Kore 独占核。

### 5.7 上报通道：supervisor → webui `/ingest`

- 每秒推送一批（NDJSON + zstd）到 webui `POST /ingest/v1/batch`。该接口
  监听**独立端口 8081**，Service 仅集群内可达，不经 Ingress。
- **只有 leader 接收上报**（§5.10）：supervisor 的上报地址是 Service
  `devpod-webui-ingest`。这个 Service 不带选择器，EndpointSlice 由 leader
  写入自己的 Pod IP。非 leader 副本收到上报时返回 503，supervisor 的环形
  缓冲负责重发。
- **令牌**：每个 DevPod 一个，值为 `HMAC-SHA256(ingest-key, DevPod UID)`。
  controller 写入 host-key Secret 的 `ingest-token` 键（挂载于
  `/etc/devpod/ingest-token`）；webui 用同一 key 重算校验，无需查表。
  key 存放于 controller 与 webui 共享的 Secret；轮换时 controller 重写
  所有 host-key Secret，supervisor 每次请求前重读令牌文件。
- 容器内 root 能读到令牌，但只能伪造本 DevPod 的容器侧数据，与「参考级」
  定位一致。
- **续传**：每批带 `boot_id` 与递增 `seq`；webui 写入 ClickHouse 成功后
  返回已确认的 seq；未确认数据留在环形缓冲重发。
- **环形缓冲按优先级分区**：审计类事件（命令、进程、`ssh_conn`）与指标类
  分开额度（合计默认 32 MiB）。积压时先丢 / 降采样指标；审计类溢出时写
  `gap` 事件。
- **配置不放 PodSpec**：ingest 地址与遥测配置写入 host-key Secret 的
  `telemetry.json`，开启功能不改变 Pod spec 哈希、不触发 DevPod 重建；
  新版 supervisor 在 Pod 下次创建时生效（与提交 2d253cf 的原则一致）。
- **NetworkPolicy**：`render.OwnerAllowNetworkPolicy` 增加一条出站规则，
  允许访问 `devpod-system`（gateway 与 webui 所在命名空间）中 webui Pod
  的 8081 端口（`isolateNetwork` 生效后才有意义，见 §2.1）。

### 5.8 交叉校验（webui leader，每分钟）

| 告警 | 条件 |
|---|---|
| `hook_missing` | 会话持续 ≥ 60 秒、pty 输入 ≥ 1 KiB，但无任何 `shell_hook` 命令事件 |
| `untracked_entry` | 出现 `PPid == 0` 的非 1 号进程 |
| `unattributed` | 本周期内新出现 ≥ 1 个无法归属的进程（告警中记录数量） |
| `agent_silent` | DevPod 处于 Running，但超过 N 分钟（默认 3）无上报 |
| `gap` / `decoder_error` | 来自 gateway 或 supervisor 的数据丢失 / 解析失败 |

阈值均可通过 webui 启动参数配置。告警写入 `alerts` 表，并在 DevPod 上写
k8s Event。leader 用 Lease 选举，失去租约立即停止定时任务。

### 5.9 可信级别

所有容器侧数据标记 `source` 为 `supervisor` 或 `tetragon`。supervisor
数据为参考级；gateway 原始流与 Tetragon 事件为可信级。

### 5.10 实时数据层（webui leader）

新 UI（`2026-11-09-devpod-console-ui-design.md`）要求界面每秒刷新，且
延迟小于 1 秒：全部 DevPod 的指标列、总览的 KPI 和节点热力条、按节点和
按用户的汇总。设计：

- **选主**：webui 各副本用 Lease 选出一个 leader，同时负责 §5.8 的定时
  任务。失去租约时立即停止定时任务、停止接收上报，并且不再写
  EndpointSlice。
- **上报只进 leader**：leader 当选后，把 `devpod-webui-ingest`（不带选择器
  的 Service）的 EndpointSlice 写成自己的 Pod IP 和 8081 端口。非 leader
  收到上报时返回 503。webui 需要新增 RBAC：`devpod-system` 命名空间里
  `endpointslices` 的 get/list/watch/create/update。
- **leader 统一写库**：每秒对每张表合并成一次插入，带
  `insert_deduplication_token`，不依赖 ClickHouse 的异步插入。supervisor
  的 seq 在写库成功后才确认。
- **内存中的全量最新状态**，只在 leader 上：
  - 每个 DevPod 的最新容器级样本，超过 5 秒没更新视为"无数据"；
  - 最新的活跃进程，以及每 5 秒一次的**全量进程快照**。全量快照由
    supervisor 每 5 秒发一次，只放在内存里，**不入库**；ClickHouse 仍然
    只写 §6.2 的活跃进程和前 N 名。
  - 由上面两项算出的汇总：
    - 按节点：结合 Node 对象和 Pod 所在节点；GPU 每卡的占用者按 supervisor
      上报的 GPU UUID 对应。webui 需要新增 `nodes` 的 get/list/watch
      权限（ClusterRole）。
    - 按用户（owner），以及 KPI 合计。
    - 只在至少有一个订阅者时才计算。
- **其他副本从 leader 拉**：非 leader 副本和 leader 之间维持一条内部长
  连接（8081 端口上的 `GET /internal/live`，认证用 ingest key 派生的令牌）。
  leader 每秒推一份完整快照，副本再分发给自己的浏览器 SSE 连接。leader
  自己直接从内存分发。
- **空转判断**："24 小时内没有会话，并且 CPU 平均低于 5%、GPU 利用率平均
  低于 5%"（UI spec §5.3）。由 leader 每分钟查一次 `*_1m` 表，结果放在
  内存快照里一起下发。
- **切换**：Lease 到期后，最多约 15 秒内产生新 leader，期间：
  - 上报由 supervisor 缓冲重发；
  - 浏览器 SSE 保持连接，界面顶部显示"实时数据重连中"；
  - 新 leader 的内存从空开始，收到第一批上报后即恢复。
  历史数据以 ClickHouse 为准，不受影响。

---

## 6. 资源指标与容器信息（二期，§6.6 为三期）

全部由 supervisor telemetry 子进程采集，不需要特权，经 §5.7 通道上报。

### 6.1 容器级指标（每 1 秒）

| 指标 | 来源（cgroup v2 优先，回退 v1） | 计算 |
|---|---|---|
| CPU | `cpu.stat`、`cpu.max`、`cpuset.cpus.effective` | 用量（核）= usage 差值 / 间隔；限额；被限流时间；cpuset 与 Kore 拓扑视图对应 |
| 内存 | `memory.current`、`memory.max`、`memory.stat`、`memory.events` | **工作集 = current − inactive_file**；anon / file / shmem 构成；`oom_kill` 次数 |
| 磁盘 IO | `io.stat` | 读写字节 / 秒、IOPS |
| 网络（内核协议栈） | 见 §6.3 | 收发字节 / 秒、包、错误、丢包 |
| RDMA | 见 §6.4 | 单独一组，不与网络相加 |
| GPU | 常驻子进程 `nvidia-smi --query-gpu=... --format=csv,noheader,nounits -lms 1000` | 每卡 UUID、型号、利用率、显存用量 / 总量、功耗、温度、SM 频率 |

### 6.2 按进程指标（每 1 秒）

| 指标 | 来源 | 精度 |
|---|---|---|
| CPU | `/proc/<pid>/schedstat`（纳秒），回退 `/proc/<pid>/stat` | 精确 |
| 内存 | 每秒 `/proc/<pid>/statm`（RSS）；每 10 秒对 RSS 前 10 读 `smaps_rollup`（PSS） | RSS 精确；PSS 10 秒粒度 |
| 磁盘 IO | `/proc/<pid>/io` 的 `read_bytes` / `write_bytes` | 精确 |
| 网络 | §6.3 的 sock_diag TCP 统计 | 仅 TCP，近似 |
| GPU | 扫描 `/proc/<pid>/fd` 找打开 `/dev/nvidia*` 的进程 | 一卡一进程时整卡数据归该进程（精确）；多进程标注「N 个进程共享」 |
| RDMA | 扫描 `/proc/<pid>/fd` 找打开 `/dev/infiniband/uverbs*` 的进程 | 只标记「在使用」，不拆字节 |

- **只上报活跃进程**：本秒有 CPU / IO / 网络用量或 RSS 变化的进程，加上
  各项指标前 N 名（默认 20）；其余合并为一行「其他」（`pid=0`）。
- 每个进程附带 §5.3 的会话 / 用户归属。

### 6.3 网络（内核协议栈）

| DevPod 类型 | 总量 | 按进程拆分 |
|---|---|---|
| 非 hostNetwork | Pod 网卡计数器 `/proc/net/dev`（排除 `lo`），精确，含 UDP | `NETLINK_SOCK_DIAG` 每秒拉取 TCP socket 的 `tcpi_bytes_acked` / `tcpi_bytes_received`，按 socket inode 归到进程；总量减 TCP 显示为「其他（UDP 等）」 |
| hostNetwork | 本容器进程的 TCP socket 字节之和（近似） | 同上 |

- 局限：仅 TCP；两次采样间建立又关闭的短连接会漏计或少计。
- **hostNetwork 的 UDP 在二期显示为「不可用（需要 node-agent）」**，三期
  由 §6.6 补齐。

### 6.4 RDMA（单独面板）

- 设备发现：容器内 `/dev/infiniband/uverbs*` →
  `/sys/class/infiniband_verbs/uverbsN/ibdev` → IB 设备；只统计这些设备。
- 每秒读 `/sys/class/infiniband/<dev>/ports/<port>/counters/` 的
  `port_xmit_data` / `port_rcv_data`（×4 字节）与包数；`hw_counters`
  存在时读取健康类指标（CNP、乱序 / 重传、链路断开），有哪些采哪些。
  GPUDirect RDMA 流量包含在内。
- 精度标签：存在 `device/physfn` 的 **SR-IOV VF** → 本容器独占，精确；
  否则 **PF 共享** → 设备级，含其他租户流量，页面明确标注。
- 部分网卡驱动下 RoCE 流量可能同时计入网卡计数器，实测后在页面注明。

### 6.5 容器内部信息

| 类别 | 频率 | 内容 |
|---|---|---|
| 静态信息 | 启动时，之后每 60 秒检查变化 | `/etc/os-release`、内核版本、cgroup 限额与 cpuset、GPU 清单（型号、显存、驱动 / CUDA 版本）、RDMA 设备、当前 shell、supervisor 版本、各采集器状态 |
| 进程概况 | 每 15 秒 | 按 CPU、按 RSS 各前 20 进程（带归属）、进程 / 线程总数（`pids.current`）、僵尸进程数 |
| 监听端口 | 每 15 秒 | `/proc/net/tcp{,6}` 中 LISTEN 端口 + 所属进程 |
| 存储用量 | 每 60 秒 | 持久化 PVC 挂载点的 `statfs`（容器根文件系统与 hostPath home 不采集） |

### 6.6 node-agent 增强（三期）

node-agent 为特权 DaemonSet（`CAP_BPF`、`CAP_NET_ADMIN`、`hostPID`、读
宿主 cgroupfs），与 §5.6 的 Tetragon 转发为同一组件：

- **按进程精确 GPU**：节点上调用 NVML 取宿主机 PID，经
  `/proc/<宿主pid>/status` 的 `NSpid` 换算为容器内 PID，得到每进程显存
  与利用率。
- **精确网络计量**：在每个 DevPod 容器 cgroup 挂 `cgroup_skb`
  ingress / egress，按 `(cgroup, TCP/UDP/其他, 方向)` 计字节与包；挂
  `cgroup/sock_create` 记录 socket cookie → 进程，实现按进程拆分（含
  UDP，hostNetwork 同样准确）。部署后对所有 DevPod 优先采用其数据。
- 可选：RDMA netlink 按 QP 统计，换算为按进程 RDMA 流量（依赖内核与
  驱动支持，实测决定）。
- 开销：每包执行一次 map 更新；RDMA 不经协议栈，不受影响。

### 6.7 GPU 的已知限制

- NVML 报告宿主机 PID，驱动不感知 PID 命名空间，容器内无法精确拆分
  每进程 GPU；`--pid=host` 打破租户隔离，不采用。三期由 node-agent 解决。
- 共享 GPU（time-slicing / MPS）时利用率是整卡数据，页面加注。
- MIG 设备利用率不可用时显示 N/A；容器内无 `nvidia-smi` 时 GPU 为 N/A。
- GPU 采集实现为 `GPUCollector` 接口，以后可替换为 NVML 直调。

### 6.8 开销预算

supervisor 运行在用户 cgroup 内（Kore 绑核时落在用户独占核上）：
目标 **平均 < 单核 1%，telemetry 子进程 RSS < 30 MB**（300 个进程、
1 秒采样）。所有间隔可配置（§10.3）。**已测（2026-11-10）：单核
0.91%、RSS 14 MB**（311 个进程、104 核节点），见
`docs/superpowers/notes/2026-11-telemetry-phase2-measurements.md`。

---

## 7. 策略（一期）

### 7.1 策略项（默认全部 allow）

| 策略项 | 取值 | 执行方式 |
|---|---|---|
| `download` | `Allow` / `Deny` | SFTP：带 READ 标志的 OPEN 不转发，直接回 `SSH_FX_PERMISSION_DENIED`（READ\|WRITE 同样视为下载；列目录不受影响）。legacy scp：拒绝 `exec scp -f`。ZMODEM：检测到 `sz` 后停止转发，向两端注入 ZMODEM 取消序列，并向终端输出一行提示 |
| `upload` | `Allow` / `Deny` | 对称：SFTP WRITE / CREAT、`scp -t`、`rz` |
| `maxFileSize` | Quantity | SFTP：读写偏移超过上限后后续请求返回失败；scp：`C` 行声明大小超限即回错误；ZMODEM：ZFILE 头大小超限即中止 |
| `forwarding.local` / `forwarding.remote` | `Allow` / `Deny` | 拒绝 direct-tcpip channel（`administratively prohibited`）/ 拒绝 `tcpip-forward` 请求。拒绝本地转发会使 VS Code Remote-SSH 不可用 |
| `forwarding.recordContent` | bool | 端口转发 channel 是否录制原始流 |
| `agentForwarding` / `x11Forwarding` | `Allow` / `Deny` | 拒绝对应 channel |
| `failMode` | `Open` / `Closed` | 见 §7.4 |

### 7.2 配置与合并

- 集群默认：`GatewayConfig.spec.audit.policy`。
- 按用户覆盖：`User.spec.auditPolicy`，**按字段覆盖**，未设置字段沿用
  集群默认。LDAP-only 用户（无 User CR）用集群默认。
- `User.spec.auditPolicy` 可由 webui 管理员修改（同配额），每次修改写
  审计事件；集群默认在页面只读。
- **按会话发起人定策略**，不按 DevPod owner（collaborator 适用自己的
  策略）。
- 会话开始时取策略快照，会话期间不变；快照记入 `sessions` 表。

### 7.3 用户反馈与记录

- 拦截时给出清晰提示，如 SFTP 错误信息
  `blocked by DevPod audit policy: downloads disabled`。
- 每次拦截写 `policy_events`（谁、操作、命中规则、处置），并累加指标。
- 配置校验：`download=Deny` 且 `forwarding.local=Allow` 时警告
  「VS Code 可经转发通道绕过下载限制」。

### 7.4 fail-open / fail-closed

| 情况 | `Open` | `Closed` |
|---|---|---|
| spool 用量 ≥ 90% | 照常 | 拒绝该用户的新会话 |
| spool 写满 | 丢弃原始流数据、写 `gap` 记录 | 断开该用户现有会话 |
| channel 解析器 panic，且该用户有 deny 类策略 | 放行 + `decoder_error` 告警 | 关闭该 channel |

事件数据在 spool 中有单独预留额度，原始流丢弃时尽量保住事件。

### 7.5 边界

策略只能管住可识别的协议（SFTP、scp、ZMODEM、端口 / agent / X11 转发）。
§1 非目标中列出的不透明传输只记录不拦截。真正的防外泄依赖 §2.1 的出站
管控。

---

## 8. 存储模型（一期起）

### 8.1 S3（bucket 默认 `devpod-audit`）

| 前缀 | 内容 | 过期 |
|---|---|---|
| `raw/<yyyy>/<mm>/<dd>/<session>/<seq>.seg.zst` | 原始流分段 | 生命周期规则，`audit.retentionDays` |
| `files/<yyyy>/<mm>/<dd>/<session>/<channel>-<n>` | 每次传输的文件原文 | 同上 |

S3 客户端统一用 aws-sdk-go-v2；大对象分片上传。gateway 凭据只需写入，
webui 凭据只需读取。

### 8.2 ClickHouse（数据库默认 `devpod_audit`）

**审计表（来源 gateway，一期）**

| 表 | 关键字段 | 引擎 / 排序键 / TTL |
|---|---|---|
| `sessions` | session_id、user、devpod、devpod_uid、owner、gateway_pod、client_ip、pubkey_fp、auth_kind、proxy_alias、backend_local_addr、opened_at、closed_at、close_reason、bytes_in、bytes_out、policy_snapshot、gaps、version | ReplacingMergeTree(version)：开始写一版、结束写完整一版；`(user, opened_at, session_id)`；TTL retentionDays |
| `channels` | session_id、channel_id、type、command / subsystem、target、pty 信息、opened_at、closed_at、exit_status、bytes_in、bytes_out、raw_recorded、version | ReplacingMergeTree(version)；`(session_id, channel_id)` |
| `commands` | event_id、ts、session_id（可为待定）、devpod_uid、user、source（`gateway_exec` / `shell_hook`）、channel_id、pid、cwd、cmdline、exit_code、duration_ms、attribution（`exact` / `inferred` / `pending` / `unattributed`）、version | ReplacingMergeTree(version)；`(devpod_uid, ts, event_id)`；cmdline 上 ngrambf_v1 跳数索引。event_id 由来源生成且稳定（gateway：会话 ID + channel 序号；钩子：`boot_id/shell pid/starttime/命令序号` 的哈希），回填时以相同 event_id 插入更高 version |
| `file_events` | ts、session_id、channel_id、user、devpod_uid、protocol、op、path、path2、size、sha256、complete、blocked、s3_key | MergeTree；`(devpod_uid, ts)`；path、sha256 上 bloom 索引 |
| `raw_segments` | session_id、seq、s3_key、first_ts、last_ts、bytes、compressed_bytes | MergeTree；`(session_id, seq)` |
| `policy_events` | ts、session_id、user、devpod_uid、rule、action、detail | MergeTree；`(ts)` |
| `alerts` | ts、devpod_uid、session_id、kind、source、detail | MergeTree；`(ts)` |
| `access_log` | ts、viewer、action、target_session、target_file、client_ip | MergeTree；`(ts)` |

**容器侧表（二期）**

| 表 | 说明 | TTL |
|---|---|---|
| `ssh_conns` | devpod_uid、boot_id、pid、starttime、peer_addr、first_seen、session_id、version | retentionDays |
| `processes` | devpod_uid、boot_id、pid、starttime、ppid、exe、cmdline、uid、first_seen、exit_seen、source、session_id、attribution、version（ReplacingMergeTree） | retentionDays |
| `container_info` | devpod_uid、boot_id、静态信息 JSON、采集器状态（ReplacingMergeTree） | retentionDays |
| `container_snapshots` | ts、devpod_uid、前 N 进程 / 监听端口 / 存储（JSON 字符串） | 7 天 |
| `metrics_container_1s` | ts、devpod_uid、boot_id、CPU / 内存 / IO / 网络各列、net_scope | 7 天 |
| `metrics_gpu_1s` | ts、devpod_uid、gpu_uuid、util、mem_used、mem_total、power、temp、sm_clock、proc_count、shared | 7 天 |
| `metrics_rdma_1s` | ts、devpod_uid、ibdev、port、tx/rx bytes、tx/rx pkts、健康计数、scope（`vf_exclusive` / `pf_shared`） | 7 天 |
| `metrics_process_1s` | ts、devpod_uid、boot_id、pid、starttime、cpu、rss、pss、io_r、io_w、tcp_rx、tcp_tx、gpu、rdma、session_id | 48 小时 |
| `*_1m` | 前三类指标表的物化视图汇总（avg / max / p95，AggregatingMergeTree） | 90 天 |

### 8.3 写入幂等

- 每批写入带 `insert_deduplication_token`（gateway：会话 ID + 批次序号；
  supervisor / node-agent：`devpod_uid/boot_id/seq`），重试不产生重复。
- 归属回填与状态更新一律**重新插入新版本行**，不用 `ALTER UPDATE`。

### 8.4 账号

| 账号 | 权限 |
|---|---|
| `devpod_gateway` | INSERT 审计表 |
| `devpod_webui` | SELECT 全部；INSERT 容器侧表、`alerts`、`access_log`、回填用的新版本行 |
| `devpod_migrate` | DDL（仅建表 Job 使用） |

ClickHouse 客户端用 clickhouse-go v2，查询一律参数化。

### 8.5 保留期（均可配置）

| 数据 | 默认 |
|---|---|
| S3 原始流与文件原文、ClickHouse 审计表与容器侧事件表 | 180 天（`audit.retentionDays`） |
| 容器级 / GPU / RDMA 指标 | 1 秒粒度 7 天；1 分钟粒度 90 天 |
| 进程级指标 | 1 秒粒度 48 小时，不汇总 |

### 8.6 容量估算与风险

- 300 个并发会话、平均 2 KB/s TTY：每天约 17 GB 原始数据，zstd 后约
  2–3 GB，180 天约 0.5 TB。
- 指标：200 个 DevPod × 每秒 1 行容器级 ≈ 每天 1700 万行；进程级按「只报
  活跃进程」控制。
- gateway 实测（2026-11-09）：开启审计后 TTY 吞吐不变，scp / sftp 单会话
  约 100 MB/s（关闭时约 140 MB/s），CPU 约 +7 ms/MiB。
- **风险**：VS Code Remote-SSH 的流量可能经 exec server（exec channel）
  而非 direct-tcpip，取决于其设置。若如此，默认策略会完整录制，容量显著
  增大。实测后若成立，将 VS Code exec server 识别为不透明通道，纳入
  `forwarding.recordContent` 同一开关。

---

## 9. 平台（webui）

### 9.1 后端新增

- ClickHouse 与 S3 客户端（§8）。
- **S3 由 webui 代理读取并流式返回**（自带 Silo 在集群内，浏览器不可达）；
  所有内容访问必经 webui，`access_log` 不漏记。
- **审计相关接口每次请求重新判定管理员身份**（`--admins` 列表 +
  `User.spec.admin`，读缓存），不信任 session cookie 中的 admin 标志位。
- `/ingest`（8081）、Lease 选主、交叉校验与回填任务（§5.7、§5.8）。
- 实时数据层（§5.10）：leader 的内存状态、副本之间的 `/internal/live`，
  以及 UI spec §6.1 的实时流。
- SSE：现有 `/api/devpods/{name}/stream` 增加 `metrics` 事件，每秒推送；
  同一副本内观看同一 DevPod 的连接共享 1 秒缓存。

### 9.2 权限（服务端执行）

| 数据 | 管理员 | 普通用户 |
|---|---|---|
| 会话、命令、文件事件、录像、文件原文 | 全部 | 只能看**自己发起**的会话；作为 owner 看不到 collaborator 的会话 |
| 原始流下载 | ✅ | ❌ |
| DevPod 监控、容器信息 | 全部 | 自己为 owner 的 DevPod |
| 进程列表 | 全部 | 自己 DevPod 的进程；属于其他用户会话的进程只显示可执行文件名与资源用量，**命令行脱敏** |
| 告警、访问记录、策略编辑、资源总览 | ✅ | ❌ |

写入 `access_log` 的操作：回放录像、下载文件原文、下载原始流、管理员
查看他人会话详情。

**在线状态**：owner 在列表页 / 详情页能看到自己 DevPod 上当前活跃会话的
数量与用户名（等同在机器上执行 `who`），但看不到 collaborator 会话的详情、
命令、录像与文件。

**隔离的边界**：owner 与 collaborator 共享同一个容器的 root，owner 在容器
内执行 `ps` 就能看到 collaborator 进程的命令行。因此进程列表的脱敏只是
平台层的一致性规则；平台真正能对 owner 隔离的，是审计数据（录像、命令
历史、文件原文）。

### 9.3 API

| 接口 | 说明 |
|---|---|
| `GET /api/audit/sessions` | 会话列表；过滤：user、devpod、时间、有文件传输、有告警 / 拦截 / gap。非管理员强制 `user=self`。**必须带时间范围**（默认最近 7 天），cursor 分页 |
| `GET /api/audit/sessions/{id}` | 详情：channel、命令、文件事件、策略事件、告警、（二期）归属进程 |
| `GET /api/audit/sessions/{id}/channels/{ch}/cast` | asciicast v2，从原始流现场转换，流式返回 |
| `GET /api/audit/sessions/{id}/channels/{ch}/raw` | 仅管理员：原始字节流 |
| `GET /api/audit/files/{event}/content` | 文件原文，webui 代理 S3 |
| `GET /api/audit/commands`、`GET /api/audit/files` | 跨会话检索（命令子串；路径 / sha256） |
| `GET /api/audit/alerts`、`GET /api/audit/access-log` | 仅管理员 |
| `GET` / `PUT /api/admin/users/{name}/audit-policy` | 管理员编辑用户策略；`GET /api/admin/audit/policy-default` 只读集群默认 |
| `GET /api/devpods/{name}/metrics?range=&step=` | 时序；超出 1 秒粒度保留期自动切到 `_1m` 表 |
| `GET /api/devpods/{name}/processes`、`GET /api/devpods/{name}/info` | 进程前 N 与容器信息 |
| ~~`GET /api/admin/metrics/overview`~~ | 改由下面的实时流和节点 / 用户接口提供 |
| `GET /api/live/devpods?scope=mine\|all` | SSE，每秒一批，运行中 DevPod 的最新样本（字段见 UI spec §6.1） |
| `GET /api/admin/live/overview` | SSE，每秒：KPI、各节点汇总、用量最大的用户、需要处理 |
| `GET /api/devpods/{name}/stream` 的 `metrics`、`processes` 事件 | 每秒；`?procs=all` 时推全量进程快照（§5.10） |
| `GET /api/admin/nodes`、`/nodes/{n}` | 节点：可分配和已分配资源、DevPod 实际用量汇总、GPU 每卡占用者 |
| `GET /api/admin/users` 的用量字段 | 每个用户的当前 CPU、内存、GPU 用量 |

接口的完整清单和字段以 UI spec §6 为准（预约和通知相关的接口属于预约
子项目）。

查询保护：必须带时间范围、最大返回行数、`max_execution_time`、每用户
并发上限。

### 9.4 页面

> **已被取代**：页面布局以 `2026-11-09-devpod-console-ui-design.md` 为准。
> 下面是一期写下的原始设想，仅作参考。

**DevPod 列表页（重构为高密度表格，二期补齐指标列）**

- 一行一个 DevPod（约 30px 行高），列：Name、Phase、CPU（60 秒小曲线 +
  用量 / 限额）、Memory（条 + 工作集 / 上限）、GPU（每卡一个格子 + 平均
  利用率）、Net ↓/↑（hostNetwork 标 `TCP only`）、RDMA ↓/↑（标 `VF` /
  `shared`）、Sess（活跃会话数与用户）、Up、Image、SSH 复制、操作。
- Pending / Stopped / Failed 行的指标列合并为一整格状态说明（拉取镜像、
  休眠时间与 PVC、失败原因与重试倒计时）。
- 顶部：名称 / 镜像过滤、Phase 过滤 chip、资源合计（对比配额）。
- 窄屏按优先级隐藏列（Image → RDMA → Net → Up），不出现横向滚动。
- 管理员全局 DevPod 视图复用同一表格组件，增加 Owner 列。

**DevPod 详情页**

- 顶部：名称、Phase、SSH 复制、Hibernate / Delete；元信息行（owner、节点、
  Pod IP、镜像、运行时长、shell、home 卷、Kore 绑定、collaborators）。
- 6 个实时指标卡（CPU、Memory、GPU、Net、RDMA、Disk IO），每卡带 60 秒
  曲线。
- 标签页：
  - **Overview**：现有 SSH 命令、SSH config、Kore 节点布局。
  - **Monitoring**：Live 1s / 15m / 1h / 24h / 7d；6 张图：CPU（用量、
    限额线、被限流）、内存（anon / file 叠加、限额线）、GPU（按卡，可切换
    显存 / 功耗 / 温度）、网络（内核协议栈，可拆 TCP / 其他）、RDMA（按
    设备，带精度标签，可切换 CNP / 重传）、磁盘 IO。
  - **Processes**：PID、命令、会话（带归属依据标签：exact / inferred /
    SSH_CONNECTION）、CPU、RSS、IO、TCP、GPU、RDMA、启动时间；1 秒刷新，
    可排序；他人会话的进程命令行脱敏。
  - **Sessions**：本 DevPod 上的会话（按 §9.2 可见范围）、回放入口、拦截
    标记。
  - **Container**：OS / 内核、限额与 cpuset、GPU、RDMA、监听端口、存储。
  - **Events**：现有事件流。

**审计页面**

- 审计 › 会话：会话列表，标记告警 / 拦截 / gap；普通用户为「我的会话」。
- 会话详情：时间线（channel、命令、文件事件、拦截、告警按时间穿插）；
  录像回放用 **asciinema-player**，**点击时间线上的命令跳转到对应时刻**；
  下载文件前提示「此操作将被记录」。
- 审计 › 命令检索、审计 › 文件传输。
- 审计 › 告警、审计 › 访问记录（仅管理员）。

**管理页面**

- 管理 › 资源总览：所有 DevPod 实时用量，可排序、按用户汇总。
- 管理 › 用户：增加审计策略编辑。

**前端依赖**：uPlot（时序图，MIT）、asciinema-player（Apache-2.0）。

---

## 10. 配置面

### 10.1 CRD 变更

```go
// GatewayConfigSpec 新增
Audit *AuditSpec `json:"audit,omitempty"`

type AuditSpec struct {
    // Enabled 开启 gateway 审计。按会话读取，修改无需重启。
    Enabled bool `json:"enabled"`
    // Notice 追加在 banner 前的录制提示；空串用默认文案。
    Notice string `json:"notice,omitempty"`
    // Policy 集群默认策略。
    Policy AuditPolicy `json:"policy,omitempty"`
}

// UserSpec 新增：按字段覆盖集群默认
AuditPolicy *AuditPolicy `json:"auditPolicy,omitempty"`

// +kubebuilder:validation:Enum=Allow;Deny
type PolicyAction string
// +kubebuilder:validation:Enum=Open;Closed
type FailMode string

type AuditPolicy struct {
    Download        *PolicyAction      `json:"download,omitempty"`
    Upload          *PolicyAction      `json:"upload,omitempty"`
    MaxFileSize     *resource.Quantity `json:"maxFileSize,omitempty"`
    Forwarding      *ForwardingPolicy  `json:"forwarding,omitempty"`
    AgentForwarding *PolicyAction      `json:"agentForwarding,omitempty"`
    X11Forwarding   *PolicyAction      `json:"x11Forwarding,omitempty"`
    FailMode        *FailMode          `json:"failMode,omitempty"`
}

type ForwardingPolicy struct {
    Local         *PolicyAction `json:"local,omitempty"`
    Remote        *PolicyAction `json:"remote,omitempty"`
    RecordContent *bool         `json:"recordContent,omitempty"`
}
```

连接信息（S3、ClickHouse）与存储参数是静态的，不放 CRD，走 chart values
→ 启动参数。

### 10.2 启动参数

| 组件 | 新增参数 |
|---|---|
| gateway | `--audit-s3-endpoint`、`--audit-s3-bucket`、`--audit-s3-region`、`--audit-s3-credentials-dir`、`--audit-clickhouse-url`、`--audit-clickhouse-database`、`--audit-clickhouse-credentials-dir`、`--audit-spool-dir`、`--audit-spool-max-bytes`（默认 20Gi） |
| webui | 同上的 S3 / ClickHouse 读参数，`--ingest-listen`（默认 `:8081`）、`--ingest-key-file`、`--leader-election-namespace` |
| controller | `--ingest-url`（空 = 不启用遥测）、`--ingest-key-file`、遥测默认值（写入 `telemetry.json`） |

chart：gateway 增加 spool emptyDir（`sizeLimit`）与
`terminationGracePeriodSeconds: 300`（不需要 preStop：gateway 收到 SIGTERM
后自行等待会话、再排空 spool）；gateway RBAC 无新增；webui RBAC
增加 `coordination.k8s.io/leases`（选主）与 `authentication.k8s.io/
tokenreviews`（三期）；`values.yaml` 增加 `audit.*`。

### 10.3 supervisor 配置（host-key Secret）

controller 在 `ensureHostKeySecret` 中**补齐并同步**以下键（现有逻辑只在
Secret 缺失时创建，需改为对这两个键做 reconcile）：

- `ingest-token`：§5.7 的 HMAC 令牌。
- `telemetry.json`：

```json
{
  "ingestURL": "http://devpod-webui.devpod-system.svc:8081/ingest/v1/batch",
  "metricsIntervalMs": 1000,
  "procScanIntervalMs": 500,
  "socketMapIntervalMs": 5000,
  "topN": 20,
  "gpu": true,
  "rdma": true,
  "processTracking": true,
  "hooks": true,
  "bufferBytes": 8388608
}
```

Secret 卷（非 subPath）更新会传播到运行中的 Pod；supervisor telemetry
定期重读。

---

## 11. 错误处理

### 11.1 gateway

- spool：emptyDir + `sizeLimit`（默认 20Gi）。80% 打告警指标；90% /
  写满的处理见 §7.4。事件有单独预留额度。
- 上传：指数退避重试；去重令牌保证幂等。事件每 5 秒或会话结束时刷写；
  原始流按 §4.3 切段。
- 重启：容器崩溃重启后 emptyDir 仍在，启动时扫描 spool 继续上传。Pod
  删除时 preStop 先传完；超时未传完的部分记 `gap` 告警。
- 解析器 panic 与策略：见 §4.6、§7.4。

### 11.2 supervisor

- telemetry 独立子进程 + `GOMEMLIMIT`（§5.5）。
- 每个采集器单独 recover；失败的采集器停用并在 `container_info` 中报告。
  `nvidia-smi` 子进程退出后退避重启；不存在时 GPU 为 N/A。
- webui 不可用：数据进环形缓冲，按优先级丢弃（§5.7），任何情况下不阻塞
  用户工作负载。
- 自动检测 cgroup v1 / v2；`/run/devpod` 不可写时回退目录。
- 老 Pod（旧版 supervisor）无遥测：页面根据 `container_info` 缺失或版本
  提示「监控在 Pod 下次重建后生效」。

### 11.3 webui

- `/ingest`：校验令牌、单批上限 4 MiB、按 DevPod 限流；ClickHouse 写入
  失败返回 503，supervisor 保留并重试。
- ClickHouse 不可用：审计页面报错；**DevPod 列表照常可用**，指标列显示
  「—」。
- 失去 Lease 立即停止定时任务。

---

## 12. 测试

| 层次 | 内容 |
|---|---|
| 单元（表驱动） | rawlog 编解码往返；asciicast 转换（golden 文件）；策略引擎（含合并规则）；会话归属规则（伪造 procfs 目录，采集器支持配置 proc 根）；cgroup v1/v2 解析；RDMA 设备发现与精度标签；权限规则 |
| 协议解析器 | 以真实客户端产生的原始流为夹具：CI 安装 OpenSSH `sftp`、`scp`、`scp -O` 与 `lrzsz`，在 pty 中运行 `sz` / `rz` 录制原始流。覆盖随机读写、中途断开、超大文件、被拦截 |
| 集成（`-tags integration`） | `hack/test-integration.sh` 在 `KUBE_CONTEXT` 指定的集群中起一次性的 ClickHouse 与 Silo Pod 并 port-forward（实现时改为此方式，不用 testcontainers）；进程内 gateway + 真实 sshd 后端，验证「会话 → spool → 上传 → 入库」全链路；故障注入：S3 / ClickHouse 断开、spool 写满、gateway 重启 |
| envtest | webui 审计 API：A 看不到 B 的会话；owner 看不到 collaborator 会话；collaborator 进程命令行脱敏；每次请求重判管理员；访问记录写入；`/ingest` 令牌校验 |
| e2e | `hack/e2e-audit.sh`：在共享集群的隔离命名空间中运行（不起第二个 controller，读自己的 GatewayConfig，见 `--gateway-config-name`）；chart 接线由 `hack/test-chart.sh` 覆盖。原计划为 kind 上部署自带 Silo + ClickHouse；执行含命令、sftp、`scp -O`、rz/sz（expect 脚本）的会话，断言 ClickHouse 行与 S3 对象；策略拦截；1 秒指标实时出现在 API |
| 前端（Vitest） | 表格单位 / 标签格式化、脱敏展示、窄屏按优先级隐藏列 |

---

## 13. 分期与验收

### 一期：gateway 审计 + 策略 + 平台审计部分

- 开启审计后，一个包含命令、sftp 上传 / 下载、`scp -O`、rz / sz 的会话：
  命令与文件事件在会话结束后 10 秒内可在 webui 检索；文件原文可下载；
  录像可回放且能从时间线跳转。
- 每项策略在 e2e 中可被触发并产生 `policy_event`，用户看到明确提示。
- 普通用户只能看到自己发起的会话；管理员的查看操作写入访问记录。
- 列表页与详情页为新布局（指标列在二期补齐），审计关闭时行为与现在一致。

### 二期：supervisor 遥测

- 实时数据层（§5.10）：上报只进 leader；leader 统一写库；其他副本从
  leader 拉；切换期间不丢历史，界面最多约 15 秒显示"重连中"。

- 新建的 DevPod 在 webui 列表与详情页显示 1 秒刷新的 CPU / 内存 / IO /
  网络 / RDMA / GPU；进程标签页显示带会话归属的进程；shell 钩子命令出现
  在会话时间线。
- tmux 中运行的进程能归属到创建它的会话（经 `SSH_CONNECTION`）。
- `kubectl exec` 进入容器产生 `untracked_entry` 告警。
- supervisor 开销满足 §6.8。

### 三期：node-agent

- 部署 node-agent 后：进程事件来源为 tetragon；每进程 GPU 显存精确；
  hostNetwork DevPod 的 UDP 流量可见；GPU 节点上训练吞吐无可测下降。

---

## 14. 实现计划阶段需实测的事项

0. （二期，新增）在 zjusct 上模拟 400 个 supervisor 每秒上报：leader 的
   CPU 和内存、每秒合并写入 ClickHouse 的耗时、界面端到端延迟（目标小于
   1 秒），以及强制切换 leader 时的恢复时间和数据完整性。**已测
   （2026-11-10）**：leader 0.72 核、RSS 约 270 MB；每秒约 9600 行，插入
   p50 140 ms；界面延迟 p50 1.12 s、p99 1.80 s（未达 1 秒目标，见记录）；
   切换后 3 秒换主、约 9 秒恢复上报、数据零丢失。chart 在开启遥测时把
   webui 资源调为 2 核 / 1 Gi。详见
   `docs/superpowers/notes/2026-11-telemetry-phase2-measurements.md`。

1. Silo 的 bucket 初始化、生命周期规则、分片上传与 aws-sdk-go-v2 的兼容
   性；镜像 digest 与发布记录一致。**已测（2026-11-09）：兼容**，见
   `docs/superpowers/notes/2026-11-audit-phase1-measurements.md`。
2. VS Code Remote-SSH 流量走 exec channel 还是 direct-tcpip（§8.6）。
   **待测**（需要用户本人参与；方法见上述记录）。
3. 开启 / 关闭审计时 gateway 吞吐（scp 大文件、`yes` 刷满 TTY）。
   **已测**：修复两处热点后，TTY 开启审计与关闭时持平；scp 约 −25%
   （原文与原始流各存一份）。详见上述记录。
4. supervisor 在 300 个进程、1 秒采样下的 CPU 与内存（§6.8）。
   **已测（2026-11-10）**：从单核 31% 优化到 0.91%，RSS 14 MB；过程与
   原因见 `docs/superpowers/notes/2026-11-telemetry-phase2-measurements.md`。
5. 200 个模拟 supervisor 每秒上报时 `/ingest` 与 ClickHouse 的压力。
   **已由第 0 项（400 个）覆盖。**
6. `nvidia-smi -lms 1000` 的稳定性与开销；MIG / time-slicing 下的输出。
   **待测**（需要 GPU 节点上的测试 Pod，用户决定延后）。
7. RoCE 流量是否同时计入网卡计数器（§6.4）。**待测**（需要 RDMA 节点，
   同上）。
8. （三期）Tetragon 事件与 `ssh_conn` 的启动时间匹配精度；GPU 节点与
   fork/exec 压力下的 Tetragon 开销（可用 `tetra` 导出的每程序 CPU 用量）；
   cgroup_skb 在高 PPS UDP 与 NCCL-over-TCP 下的开销。

---

## 15. 后续（不在本 spec 范围）

- §2.1 的三项前置依赖。
- 按 DevPodTemplate 配置审计策略。
- 会话实时旁观、管理员强制断开会话。
- NVML 直调替代 `nvidia-smi`（容器内）。
- 计费级指标（kubelet / cAdvisor / DCGM 作为可信来源）。
