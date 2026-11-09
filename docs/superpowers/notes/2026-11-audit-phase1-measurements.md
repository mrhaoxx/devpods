# 审计一期实测记录（spec §14 第 1–3 项）

日期：2026-11-09　分支：`audit-phase1`　集群：zjusct（隔离命名空间
`devpod-audit-it`，见 `hack/e2e-audit.sh`）

## 1. Silo 兼容性（§14.1）

- 镜像：`docker.io/pgsty/silo:RELEASE.2026-10-26T00-00-00Z`（amd64 + arm64），
  与 `deploy/chart/values.yaml`、`hack/integration/fixtures.yaml` 一致。
- `KUBE_CONTEXT=zjusct bash hack/test-integration.sh` 全部通过；对一个刚
  重建、数据为空的 Silo Pod 单独复跑：
  - `TestS3_PutGetMultipart`：PASS（aws-sdk-go-v2 manager 分片上传，
    checksum 为 WhenRequired）；
  - `TestS3_EnsureBucketAndLifecycleIdempotent`：PASS（建 bucket 与生命周期
    规则，重复执行幂等）。
- e2e（`hack/e2e-audit.sh`）中 `devpod-webui migrate --create-bucket
  --manage-lifecycle` 对 Silo 执行成功，会话结束后对象可经 webui 读取
  （cast 接口）。

## 2. VS Code Remote-SSH 流量路径（§14.2 / §8.6）

**未测，需要用户本人参与。** 方法：

1. 默认设置的 VS Code 经 gateway 连接一个开启审计的 DevPod，打开一个
   目录，正常使用约 10 分钟；
2. 在 ClickHouse 中按通道类型汇总该会话：

   ```sql
   SELECT type, subsystem, command, count() AS n,
          sum(bytes_in) AS in_b, sum(bytes_out) AS out_b
   FROM devpod_audit.channels FINAL
   WHERE session_id = '<会话 ID>'
   GROUP BY type, subsystem, command ORDER BY out_b DESC;
   ```

3. 如果大部分字节落在 `session` 类型的 exec 通道（VS Code exec server）上，
   就按 §8.6 另开任务，把它识别为不透明通道，由
   `forwarding.recordContent` 统一控制；如果落在 `direct-tcpip` 上，
   默认策略（不录转发内容）已经覆盖。

## 3. gateway 吞吐：开启 / 关闭审计（§14.3）

### 环境

- 客户端（OpenSSH）与 gateway 运行在同一个 runner Pod 中，经回环连接；
  后端是同节点上的 debian sshd Pod。runner 为 BestEffort，CPU 不受限。
- 审计存储：同命名空间中的单副本 ClickHouse 与 Silo。
- `gateway_cpu`：gateway 进程 `/proc/<pid>/stat` 中 utime + stime 的增量，
  覆盖两段 SSH 加解密和全部审计工作。运行只有 2–5 秒，`kubectl top`
  的采样窗口太粗，所以没有用。
- 负载：
  - scp：`scp -O` 上传随机数据文件；
  - TTY：`ssh -tt … 'head -c 200000000 /dev/zero | tr "\000" y'`，即 200 MB
    不带换行的 TTY 输出。
- 偏离计划：
  - 在 zjusct 上测，不在 kind 上测（用户指示）；
  - scp 文件用 512 MiB / 256 MiB，不用 1 GiB：测试用 Silo 是 4 Gi 的
    emptyDir，审计开启时每次 scp 都会存两份（文件原文和原始流）。
- 每种配置跑两轮，下表给出两轮的范围。

### 修复前（`a170c1b`，scp 512 MiB）

| | scp MB/s | scp CPU（ms/MiB） | TTY MB/s | TTY CPU（s / 200 MB） |
|---|---|---|---|---|
| 关闭审计 | 147–150 | 14–17 | 102–121 | 3.3–4.2 |
| 开启审计 | 111–115 | 24.6–24.8 | **41–43** | **10.4–10.8** |

在 Linux 上抓的 pprof：TTY 负载下，ZMODEM 解码器逐字节跑状态机，占
gateway CPU 的 **34%**，GC 另占 18%；每个 segment 新建的 zstd 编码器会按
GOMAXPROCS 起后台 goroutine 并分配缓冲。

### 修复

- `d48210d`：ZMODEM 解码器不在帧内时，只有 ZPAD（`*`）和 ZDLE 有意义，
  其余字节用 `IndexAny` 批量跳过；不含这两种字节的块原样返回，不复制。
- `d8ec631`：zstd 编码器改为同步模式（concurrency 1 + 低内存），通过
  `sync.Pool` 跨 segment 和会话复用。

本地基准（`go test ./internal/audit -bench Channel_`，Apple M 系列）：

| | 修复前 | 修复后 |
|---|---|---|
| TTY 4 KiB 块：进程 CPU / op | 21.5 µs | 2.4 µs |
| TTY 4 KiB 块：分配 / op | 22 KB，12 次 | ≈ 18 B，0 次 |
| exec 随机数据 32 KiB 块：进程 CPU / op | 16.4 µs | 6.9 µs |
| exec 随机数据 32 KiB 块：分配 / op | 75 KB | ≈ 60 B |

### 修复后（`d8ec631`，scp 256 MiB）

| | scp MB/s | scp CPU（ms/MiB） | TTY MB/s | TTY CPU（s / 200 MB） |
|---|---|---|---|---|
| 关闭审计 | 131–142 | 13.2–15.1 | 99–121 | 3.4–4.2 |
| 开启审计 | 102–105 | 21.4–21.8 | **95–111** | **3.6–4.5** |

### 结论

- TTY：开启审计后的吞吐和 CPU 与关闭时相同，差异在测量噪声以内。按
  §8.6 的估算（300 个会话、每个 2 KB/s），TTY 审计的 CPU 开销可以忽略。
- scp / sftp：开启审计后吞吐约 −25%，CPU 约 +7 ms/MiB。原因是设计本身：
  文件原文和原始流各写一份 spool 并各上传一次，另外还要算一次 SHA-256。
  单会话仍在 100 MB/s 以上，暂不处理。如果以后要省，可以考虑对已经
  捕获了文件内容的通道不再录原始流的数据部分，但这需要改 spec（录像回放
  依赖原始流）。
- 测试中出现过一次 spool 超限驱逐：Silo（emptyDir）被驱逐后上传停滞，
  而 e2e runner 用了 20Gi 的 spool 默认上限，emptyDir 只有 2Gi。
  `817afd9` 把 e2e 的 spool 上限设为 1536Mi。chart 里两者都取
  `audit.spool.sizeLimit`，不受影响。

## 4. gateway 内存：并发会话（最终审查后补测）

审查者发现每个打开的 segment 持有一个 zstd 编码器：默认级别和窗口下，
写入约 1 MiB 后占 9.6 MiB 堆，150 个会话就会超过 512Mi 的限额。
`985b8c4` 改为 SpeedFastest 加 256 KiB 窗口后，单 segment 降到 0.75 MiB
（`TestSegment_OpenWriterHeapBounded`）。

zjusct 实测：150 个并发 pty 会话，每个输出约 1.4 MB 的 base64 文本后
空闲；每 0.1 s 启动一个；取 gateway 的 VmRSS。

| | 空闲 | 150 个会话 | 每会话 |
|---|---|---|---|
| 关闭审计 | 68 MB | 145 MB | ≈ 0.5 MB |
| 开启审计 | 69–71 MB | 301–338 MB | ≈ 1.6–1.8 MB |

- 按修复前每 segment 9.6 MiB 估算，150 个会话会到 1.4 GB 以上。
- 两副本中一个重启时，另一个要承接全部会话（约 300 个，约 640 MB）。
  `b7bc600` 之后的 chart 把 gateway 内存上限默认值提到 1Gi，并把
  `GOMEMLIMIT` 设为上限的 90%。
