# DevPod 二期遥测 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** supervisor collects container-level, process-level, network, RDMA and GPU metrics every second, attributes processes to SSH sessions and records shell commands; the webui leader ingests the data, writes ClickHouse and serves the console UI's sub-second live streams and history APIs.

**Architecture:** A new `devpod-supervisor telemetry` child process (supervised by PID 1) samples cgroup/procfs/sysfs/nvidia-smi, keeps a priority ring buffer and posts NDJSON+zstd batches to the webui ingest Service. Only the Lease-elected webui leader accepts ingest (it writes its own Pod IP into the selectorless Service's EndpointSlice), merges all DevPods' rows into one ClickHouse insert per table per second, keeps the full latest state in memory, computes node/user/KPI rollups and pushes a per-second snapshot to followers over `/internal/live`; every replica fans snapshots out to browser SSE.

**Tech Stack:** Go 1.26, controller-runtime / client-go (`tools/leaderelection`, `discovery/v1` EndpointSlice), ClickHouse 26.8 via clickhouse-go v2, klauspost zstd, fsnotify (inotify), vishvananda/netlink (sock_diag), Helm chart, bash.

**Spec:** `docs/superpowers/specs/2026-11-09-devpod-audit-telemetry-design.md` (phase 2: §5.1–5.5, §5.7–5.10, §6.1–6.5, §6.7–6.8, §8.2 container tables, §9.1–9.3, §10.2–10.3, §11.2–11.3, §13 二期, §14 items 0, 4–7). Live API fields: `docs/superpowers/specs/2026-11-09-devpod-console-ui-design.md` §6.1 and §6.3.

## Global Constraints

- Go toolchain 1.26 (go.mod, CI, Dockerfiles already on 1.26).
- New Go dependencies allowed: `github.com/vishvananda/netlink` (Apache-2.0), `github.com/fsnotify/fsnotify` (promote from indirect). Nothing else without a ledger ruling.
- Telemetry config lives in the per-DevPod host-key Secret (keys `ingest-token`, `telemetry.json`, mounted under `/etc/devpod/`); enabling telemetry must not change the Pod spec or its spec-hash.
- `telemetry.json` defaults: `metricsIntervalMs 1000`, `procScanIntervalMs 500`, `socketMapIntervalMs 5000`, `topN 20`, `gpu true`, `rdma true`, `processTracking true`, `hooks true`, `bufferBytes 33554432`; plus `hostNetwork` per DevPod and `ingestURL`.
- Ingest: `POST /ingest/v1/batch` on port **8081**, Service `devpod-webui-ingest` (selectorless), token `hex(HMAC-SHA256(ingest-key, DevPod UID))`, request body limit **4 MiB**, non-leader → **503**.
- Hook files: `/run/devpod/hooks/<pid>`, fallback `/tmp/.devpod/hooks/<pid>`; fields separated by `\x1f`, records end with `\0`; a file over **1 MiB** is truncated and a `gap` alert is emitted.
- Process identity is always `(pid, starttime)`. Legit sshd-session = exe `/opt/devpod/libexec/sshd-session` **and** parent is the supervisor-started sshd.
- Full process snapshot every **5 s**, memory only (never written to ClickHouse); ClickHouse gets active processes plus top **20**.
- Live state: a DevPod sample older than **5 s** counts as "no data"; Lease duration 15 s, renew deadline 10 s, retry period 2 s.
- Idle definition (UI spec §5.3): no session for 24 h **and** CPU average < 5 % **and** GPU utilisation average < 5 %; thresholds are flags.
- Retention defaults: 1 s container/GPU/RDMA metrics 7 days, 1 m rollups 90 days, process 1 s metrics 48 hours, container-side event tables `audit.retentionDays`, `container_snapshots` 7 days.
- Overhead budget (§6.8): telemetry average < 2 % of one core and RSS < 30 MB with 300 processes at 1 s sampling.
- Permissions (§9.2): DevPod metrics/info visible to the owner and admins; non-admins see other users' processes with cmdline replaced by the exe basename.
- Tests that need a cluster run against `KUBE_CONTEXT=zjusct` in namespace `devpod-audit-it` only; never touch `devpod-system`, `devpods`, or GatewayConfig/default.

## Review Focus

- Non-root user image or `hidepid` procfs: `/proc/<pid>/environ` and `fd/` unreadable → attribution falls back to tree / SID / inferred, collectors keep running, nothing crashes → `TestAttributor_UnreadableEnvironFallsBack` in Task 4.
- PID reuse and re-parenting (tmux server daemonised, nohup, pid wraps) → identity by `(pid, starttime)`, sticky attribution survives re-parenting to PID 1, a reused pid gets a fresh attribution → `TestAttributor_StickyAcrossReparentAndPidReuse` in Task 4.
- webui leader failover mid-ingest → supervisor resends unacked batches to the new leader, no seq is lost and replays are de-duplicated; the old leader rejects with 503 the moment it loses the lease → `TestIngest_FailoverResendNoLossNoDup` in Task 18.
- cgroup v1 nodes or missing controller files (no `io.stat`, `cpu.max` = `max`, no `memory.events`) → the sample carries what exists, limits read as "unlimited", no crash → `TestCgroup_V1AndMissingFiles` in Task 3.
- hostNetwork DevPod → network total is the sum of the container's TCP socket bytes, `netKind = "tcp-only"`, `/proc/net/dev` is not used (it would show the host) → `TestNet_HostNetworkUsesTCPSum` in Task 6.

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/telemetry/wire/wire.go` | Batch/record types, NDJSON+zstd encode/decode, `Token()` |
| `internal/telemetry/agent/config.go` | `Config` + `LoadConfig`, defaults, periodic reload |
| `internal/telemetry/agent/cgroup.go` | cgroup v1/v2 container metrics |
| `internal/telemetry/agent/procfs/scan.go` | `/proc` scanner: process table, per-process metrics |
| `internal/telemetry/agent/procfs/attrib.go` | §5.3 attribution engine, `ssh_conn`, `untracked_entry` |
| `internal/telemetry/agent/procfs/sockets.go` | `/proc/<pid>/net/tcp{,6}` parsing, socket-inode → process map |
| `internal/telemetry/agent/procfs/sockdiag.go` | `TCPStats` interface + netlink sock_diag implementation |
| `internal/telemetry/agent/net.go` | container network totals (pod NIC vs hostNetwork TCP sum) |
| `internal/telemetry/agent/rdma.go` | RDMA device discovery + counters + VF/PF scope |
| `internal/telemetry/agent/gpu.go` | `GPUCollector`, `nvidia-smi -lms` collector, per-process GPU via fds |
| `internal/telemetry/agent/hooks.go` + `hookscripts.go` | shell hook scripts (bash/zsh/fish) and the inotify reader |
| `internal/telemetry/agent/info.go` | container info, snapshots (top procs, listen ports, storage) |
| `internal/telemetry/agent/buffer.go` | priority ring buffer (audit / metrics partitions) |
| `internal/telemetry/agent/uploader.go` | batching, POST, ack, retry |
| `internal/telemetry/agent/agent.go` | the telemetry run loop wiring every collector |
| `cmd/supervisor/main.go`, `cmd/supervisor/telemetry.go` | `telemetry` subcommand; PID 1 supervises it |
| `cmd/supervisor/shell.go` | install hook scripts with the existing profile scripts |
| `internal/render/telemetry.go` | `TelemetryConfig` rendering for the host-key Secret |
| `internal/render/networkpolicy.go` | egress rule to webui :8081 |
| `internal/controllers/devpod_controller.go` | reconcile `ingest-token` / `telemetry.json` keys |
| `cmd/controller/main.go` | `--ingest-url`, `--ingest-key-file`, `--telemetry-*` flags |
| `internal/audit/store/migrations/0002_telemetry.sql` | container-side tables, 1 m materialized views |
| `internal/audit/store/telemetry.go` | row types, `InsertTelemetry`, retention |
| `internal/audit/store/telemetry_query.go` | history, info, idle, backfill, cross-check queries |
| `internal/webui/leader/` | Lease elector, EndpointSlice publisher, minute jobs |
| `internal/webui/ingest/` | ingest handler, per-second merging writer, session resolver |
| `internal/webui/live/` | in-memory state, rollups, snapshot type, follower client, hub |
| `internal/webui/api_live.go`, `api_metrics.go`, `api_nodes.go` | browser-facing endpoints |
| `cmd/webui/telemetry.go` | flags and wiring |
| `deploy/chart/templates/{webui.yaml,webui-ingest.yaml,ingest-key.yaml,webui-rbac.yaml,controller.yaml}` | chart wiring |
| `hack/telemetry-load/main.go` | 400-supervisor load generator |
| `hack/e2e-telemetry.sh` | isolated end-to-end on zjusct |
| `docs/superpowers/notes/2026-11-telemetry-phase2-measurements.md` | §14 results |

---

### Task 1: Wire protocol

**Files:**
- Create: `internal/telemetry/wire/wire.go`, `internal/telemetry/wire/wire_test.go`

**Interfaces:**
- Produces:
  - `type Header struct { V int; DevPodUID, BootID string; Seq uint64; TS time.Time; AgentVersion string }` (json `v,devpod_uid,boot_id,seq,ts,agent_version`)
  - `type Record struct { K Kind; ... }` with `Kind` constants: `KContainer "c"`, `KGPU "g"`, `KRDMA "r"`, `KProc "p"`, `KProcEvent "pe"`, `KSSHConn "sc"`, `KCommand "cmd"`, `KInfo "info"`, `KSnapshot "snap"`, `KFullProcs "full"`, `KAlert "alert"`; payload structs `Container`, `GPU`, `RDMA`, `Proc`, `ProcEvent`, `SSHConn`, `Command`, `Info`, `Snapshot`, `FullProcs`, `Alert` (fields mirror spec §8.2 container tables; `Container` carries `NetKind string` = `"pod"|"tcp-only"`, `RDMA.Scope` = `"vf_exclusive"|"pf_shared"`)
  - `func (k Kind) Audit() bool` — true for `pe, sc, cmd, alert`
  - `func Encode(w io.Writer, h Header, recs []Record) error` (zstd-compressed NDJSON: header line, then one record per line)
  - `func Decode(r io.Reader, maxBytes int64) (Header, []Record, error)` — returns `ErrTooLarge` past `maxBytes` decompressed
  - `type Ack struct { AckedSeq uint64 \`json:"acked_seq"\` }`
  - `func Token(key []byte, devpodUID string) string` — hex HMAC-SHA256

- [ ] **Step 1: Write the failing tests**

```go
func TestWire_RoundTrip(t *testing.T)            // every Kind survives Encode→Decode; header fields equal
func TestWire_DecodeRejectsOversize(t *testing.T) // 5 MiB of records with maxBytes 4<<20 → errors.Is(err, ErrTooLarge)
func TestWire_UnknownKindIgnored(t *testing.T)    // a line {"k":"zz"} is skipped, not an error (forward compat)
func TestToken_StableAndKeyed(t *testing.T) {
	a := Token([]byte("k1"), "uid-1")
	if a != Token([]byte("k1"), "uid-1") || a == Token([]byte("k2"), "uid-1") || a == Token([]byte("k1"), "uid-2") {
		t.Fatal("token must depend on key and uid only")
	}
	if len(a) != 64 { t.Fatalf("hex sha256, got %d chars", len(a)) }
}
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/wire/` — Expected: FAIL (package missing).
- [ ] **Step 3: Implement** `wire.go`. Records are a tagged union: `Record{K Kind; C *Container; G *GPU; ...}` with `json:",omitempty"` pointers; Decode wraps the zstd reader in an `io.LimitReader(maxBytes+1)`.
- [ ] **Step 4: Run** `go test ./internal/telemetry/wire/` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: wire protocol"`

---

### Task 2: telemetry subcommand, PID 1 supervision, config

**Files:**
- Create: `internal/telemetry/agent/config.go`, `config_test.go`, `cmd/supervisor/telemetry.go`
- Modify: `cmd/supervisor/main.go` (start + restart the telemetry child; it never decides the supervisor's exit)
- Test: `cmd/supervisor/main_test.go`

**Interfaces:**
- Produces:
  - `type Config struct { IngestURL string; MetricsInterval, ProcScanInterval, SocketMapInterval time.Duration; TopN int; GPU, RDMA, ProcessTracking, Hooks, HostNetwork bool; BufferBytes int64 }` (json names as Global Constraints, durations as `*Ms` ints)
  - `func LoadConfig(path string) (Config, error)` — missing file → `ErrNoConfig`; missing fields → defaults
  - `func DefaultConfig() Config`
  - `const ConfigPath = "/etc/devpod/telemetry.json"`, `TokenPath = "/etc/devpod/ingest-token"`
  - `cmd/supervisor`: `func runTelemetry() int`; PID 1 starts `/opt/devpod/devpod-supervisor telemetry` only when `ConfigPath` exists, with env `GOMEMLIMIT=24MiB`, restarts it with backoff 1 s → 60 s (reset after 10 min up), never forwards its exit into `waitForChildren`'s decision.

- [ ] **Step 1: Write the failing tests**

```go
func TestLoadConfig_DefaultsAndOverrides(t *testing.T) // {"topN":5,"hostNetwork":true} → TopN 5, HostNetwork true, MetricsInterval 1s, BufferBytes 33554432
func TestLoadConfig_Missing(t *testing.T)              // errors.Is(err, ErrNoConfig)
func TestSupervisor_TelemetryCrashDoesNotExit(t *testing.T) // fake telemetry child exits 1 three times; supervisor keeps waiting on sshd, restarts with growing delay
func TestSupervisor_NoConfigNoTelemetry(t *testing.T)       // config absent → no telemetry child started
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/ ./cmd/supervisor/` — Expected: FAIL.
- [ ] **Step 3: Implement.** In `main.go` add a `restartable` child kind tracked outside `children` (so `waitForChildren` rules stay unchanged); the reaper loop already reaps all pids — route the telemetry pid's exit to the restarter. `runTelemetry` loads config and calls `agent.Run(ctx, cfg)` (Task 12; until then a stub that blocks on ctx).
- [ ] **Step 4: Run** `go test ./internal/telemetry/agent/ ./cmd/supervisor/` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "supervisor: telemetry subcommand supervised by PID 1"`

---

### Task 3: cgroup collector

**Files:**
- Create: `internal/telemetry/agent/cgroup.go`, `cgroup_test.go`, `testdata/cgroup/{v2,v1}/…`

**Interfaces:**
- Produces:
  - `type Cgroup struct{ /* root dir, version */ }`, `func OpenCgroup(root string) (*Cgroup, error)` — detects v2 (`cgroup.controllers` present) vs v1 (per-controller dirs `cpu,cpuacct`, `memory`, `blkio`, `pids`)
  - `func (c *Cgroup) Sample(prev *CgroupSample, now time.Time) (CgroupSample, error)` — `CgroupSample{CPUCores float64 (usage delta / interval), CPULimit float64 (0 = unlimited), ThrottledUsec uint64, Cpuset string, MemWorkingSet, MemAnon, MemFile, MemShmem, MemLimit (0 = unlimited) uint64, OOMKills uint64, IOReadBps, IOWriteBps, IOReadIops, IOWriteIops float64, Pids uint64}`
  - Working set = `memory.current − inactive_file` (v1: `usage_in_bytes − total_inactive_file`).

- [ ] **Step 1: Write the failing tests**

```go
func TestCgroup_V2(t *testing.T)   // fixture: usage_usec 1_000_000 → 1_500_000 over 1s → CPUCores 0.5; cpu.max "200000 100000" → CPULimit 2; memory.current 1000, inactive_file 200 → MemWorkingSet 800
func TestCgroup_V1AndMissingFiles(t *testing.T) // v1 fixture without blkio and with cfs_quota_us -1 → CPULimit 0, IO fields 0, err == nil; v2 fixture without io.stat / memory.events → no error
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/ -run Cgroup` — Expected: FAIL.
- [ ] **Step 3: Implement** with a file-reading helper that treats `ENOENT` as "absent" (zero value), never an error.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: cgroup v1/v2 collector"`

---

### Task 4: procfs scanner and attribution

**Files:**
- Create: `internal/telemetry/agent/procfs/scan.go`, `attrib.go`, `scan_test.go`, `attrib_test.go`, `testdata/proc/…` (fake proc trees built by a test helper)

**Interfaces:**
- Produces:
  - `type PID struct{ Pid int; Start uint64 }` (starttime in clock ticks)
  - `type Proc struct{ ID, Parent PID; Exe, Cmdline string; UID int; SID, TTY int; SSHConnection string; CPUNs, RSS, PSS, IORead, IOWrite uint64 }`
  - `type Scanner struct{ Root string }`; `func (s *Scanner) Scan() (map[PID]*Proc, error)` — unreadable per-process files (EACCES/ENOENT) leave fields empty, never fail the scan
  - `type Attribution struct{ Conn PID; How string }` — `How` ∈ `"tree"|"ssh_connection"|"sid_tty"|"inferred"|"unattributed"`
  - `type Attributor struct{ SSHDPid int; SessionExe string }` (SessionExe `/opt/devpod/libexec/sshd-session`)
  - `func (a *Attributor) Update(procs map[PID]*Proc, conns map[PID]string /* sshd-session → peer addr, Task 5 */) (events []wire.ProcEvent, attrib map[PID]Attribution)` — emits `process_start` / `process_exit` / `untracked_entry`; rules in spec §5.3 order; sticky: first attribution is kept for the life of `(pid,starttime)`, children inherit; `untracked_entry` when `PPid == 0` and `pid != 1`.
  - Per-process CPU from `schedstat` (fallback `stat` utime+stime ticks), RSS from `statm`, PSS from `smaps_rollup` every 10 s for the top 10 by RSS, IO from `io`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAttributor_TreeRule(t *testing.T)                    // bash child of legit sshd-session → How "tree", Conn = session pid
func TestAttributor_FakeSessionExeRejected(t *testing.T)      // process named sshd-session but exe /tmp/sshd-session, or parent ≠ SSHDPid → not "tree"
func TestAttributor_SSHConnectionForDaemonised(t *testing.T)  // tmux server reparented to 1, environ SSH_CONNECTION="10.0.0.9 41000 …" matching a conn peer → "ssh_connection"
func TestAttributor_StickyAcrossReparentAndPidReuse(t *testing.T) // attribution kept after reparent to 1; same pid with new starttime → fresh attribution + process_exit for the old one
func TestAttributor_UnreadableEnvironFallsBack(t *testing.T)  // environ EACCES: SID matching an attributed process → "sid_tty"; single active conn → "inferred"; none → "unattributed"
func TestAttributor_UntrackedEntry(t *testing.T)              // PPid 0, pid 77 → one untracked_entry event, only once per (pid,starttime)
func TestScanner_PerProcessMetrics(t *testing.T)              // schedstat "123456789 0 0" → CPUNs 123456789; statm pages × pagesize → RSS
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/procfs/` — Expected: FAIL.
- [ ] **Step 3: Implement** scanner and attributor; the scanner root is a parameter so tests use a fake tree.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: procfs scanner and session attribution"`

---

### Task 5: sockets, ssh_conn, per-process TCP

**Files:**
- Create: `internal/telemetry/agent/procfs/sockets.go`, `sockdiag.go`, `sockets_test.go`
- Modify: `go.mod` (add `github.com/vishvananda/netlink`)

**Interfaces:**
- Consumes: `Proc`, `PID` (Task 4).
- Produces:
  - `func ParseProcNetTCP(r io.Reader) ([]TCPSock, error)` — `TCPSock{Inode uint64; Local, Remote netip.AddrPort; State uint8}` (v4 and v6 hex formats)
  - `type SocketMap struct{...}`; `func BuildSocketMap(root string, procs map[PID]*Proc) (*SocketMap, error)` — inode → PID from `/proc/<pid>/fd` links `socket:[N]`
  - `func SSHConns(root string, sm *SocketMap, sessions []PID) map[PID]string` — peer address (`ip:port`) of each legit sshd-session, read from **that process's** `/proc/<pid>/net/tcp{,6}`
  - `type TCPStats interface{ Bytes() (map[uint64]TCPBytes, error) }` (`TCPBytes{Acked, Received uint64}` by inode); `NewSockDiag() TCPStats` via `netlink.SocketDiagTCPInfo` for AF_INET and AF_INET6
  - `func PerProcessTCP(prev, cur map[uint64]TCPBytes, sm *SocketMap, dt time.Duration) map[PID]struct{ Rx, Tx float64 }`

- [ ] **Step 1: Write the failing tests**

```go
func TestParseProcNetTCP_V4V6(t *testing.T)   // fixture lines → Remote "10.78.3.4:41000", LISTEN state 0x0A recognised
func TestSSHConns_PeerFromSessionNetns(t *testing.T) // fake root: session pid 50 fd 3 → socket:[999]; /proc/50/net/tcp has inode 999 remote 10.0.0.9:41000 → map{50: "10.0.0.9:41000"}
func TestPerProcessTCP_Deltas(t *testing.T)   // inode 7 acked 1000→3000 in 1s owned by pid 9 → Tx 2000; closed socket (missing in cur) contributes nothing
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/procfs/ -run 'TCP|SSHConns'` — Expected: FAIL.
- [ ] **Step 3: Implement**; `NewSockDiag` is not unit-tested (needs netlink) and is exercised by Task 23's e2e.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: socket map, ssh_conn and per-process TCP"`

---

### Task 6: container network totals

**Files:**
- Create: `internal/telemetry/agent/net.go`, `net_test.go`

**Interfaces:**
- Consumes: `PerProcessTCP` output (Task 5), `Config.HostNetwork` (Task 2).
- Produces: `func NetTotals(root string, hostNetwork bool, tcp map[procfs.PID]struct{Rx, Tx float64}, prev *NetCounters, dt time.Duration) (NetSample, NetCounters, error)` — `NetSample{Rx, Tx, TCPRx, TCPTx, OtherRx, OtherTx, RxPkts, TxPkts, Errs, Drops float64; Kind string}`; non-hostNetwork: totals from `/proc/net/dev` minus `lo`, `Other = total − TCP` (floored at 0), `Kind "pod"`; hostNetwork: totals = TCP sum, `Other*` = 0, `Kind "tcp-only"`.

- [ ] **Step 1: Write the failing tests**

```go
func TestNet_PodUsesNetDevMinusLo(t *testing.T)    // eth0 rx 1000→4000 over 1s, lo ignored, TCP rx 2000 → Rx 3000, OtherRx 1000, Kind "pod"
func TestNet_HostNetworkUsesTCPSum(t *testing.T)   // hostNetwork=true, /proc/net/dev with host-sized counters → Rx == TCP sum, Kind "tcp-only"
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/ -run Net_` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: container network totals"`

---

### Task 7: RDMA collector

**Files:**
- Create: `internal/telemetry/agent/rdma.go`, `rdma_test.go`, `testdata/rdma/…`

**Interfaces:**
- Produces: `func DiscoverRDMA(devRoot, sysRoot string) ([]RDMADev, error)` (`/dev/infiniband/uverbs*` → `sys/class/infiniband_verbs/uverbsN/ibdev` → ports; `Scope` `vf_exclusive` when `sys/class/infiniband/<dev>/device/physfn` exists, else `pf_shared`); `func (d RDMADev) Sample(sysRoot string, prev *RDMACounters, dt time.Duration) ([]wire.RDMA, RDMACounters, error)` — bytes = `port_*_data × 4`, packets, and every file present under `hw_counters` copied into a `Health map[string]uint64`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRDMA_DiscoverOnlyContainerDevices(t *testing.T) // sysfs has mlx5_0, mlx5_1; /dev/infiniband has uverbs0 → only mlx5_0
func TestRDMA_ScopeAndBytes(t *testing.T)                // physfn present → vf_exclusive; xmit_data 100→350 over 1s → TxBps 1000
func TestRDMA_NoDevices(t *testing.T)                    // no /dev/infiniband → empty, nil error
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/ -run RDMA` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: RDMA collector"`

---

### Task 8: GPU collector

**Files:**
- Create: `internal/telemetry/agent/gpu.go`, `gpu_test.go`

**Interfaces:**
- Produces:
  - `type GPUCollector interface{ Latest() ([]wire.GPU, bool); Close() }`
  - `func NewNvidiaSMI(ctx context.Context, path string) GPUCollector` — runs `nvidia-smi --query-gpu=uuid,name,utilization.gpu,memory.used,memory.total,power.draw,temperature.gpu,clocks.sm --format=csv,noheader,nounits -lms 1000`, parses each line set, restarts with backoff 1 s → 60 s when the process exits; `[N/A]` fields → `-1`; binary missing → `Latest()` returns `(nil, false)` forever
  - `func ProcGPUs(root string, procs map[procfs.PID]*procfs.Proc) map[procfs.PID][]int` — minor numbers of `/dev/nvidia<N>` opened by each process; a GPU opened by more than one process is marked `Shared` in the GPU sample

- [ ] **Step 1: Write the failing tests**

```go
func TestNvidiaCSV_Parse(t *testing.T)        // "GPU-ab, NVIDIA A100, 97, 37000, 81920, 250.3, 61, 1410" → util 97, mem used/total in MiB, power 250.3
func TestNvidiaCSV_NotAvailableFields(t *testing.T) // "[N/A]" util (MIG) → -1, not an error
func TestNvidiaSMI_RestartsOnExit(t *testing.T) // fake nvidia-smi script that prints two lines then exits → collector restarts it (seen twice within 3s)
func TestNvidiaSMI_Missing(t *testing.T)      // path does not exist → Latest() (nil,false), no busy loop
func TestProcGPUs_FdScan(t *testing.T)        // pid 4 fd → /dev/nvidia0 and /dev/nvidia1 → [0 1]
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/ -run 'Nvidia|ProcGPUs'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: GPU collector via nvidia-smi"`

---

### Task 9: shell hooks

**Files:**
- Create: `internal/telemetry/agent/hookscripts.go`, `hooks.go`, `hooks_test.go`
- Modify: `cmd/supervisor/shell.go` (`installProfileScripts` also writes the hook scripts: bash/zsh hook appended to `/etc/profile.d/devpod.sh` and the zsh bridge, fish to `/etc/fish/conf.d/devpod.fish`), `cmd/supervisor/shell_test.go`

**Interfaces:**
- Produces:
  - `const BashZshHook, FishHook string` — use only shell builtins (`printf`, parameter expansion); write to `${DEVPOD_HOOK_DIR}/$$` where the dir is `/run/devpod/hooks` if writable at shell start else `/tmp/.devpod/hooks`; record = `shell_pid \x1f shell_starttime \x1f tty \x1f cwd \x1f start_ns \x1f end_ns \x1f exit \x1f cmd \0`; bash: `trap DEBUG` + `PROMPT_COMMAND`; zsh: `add-zsh-hook preexec/precmd`; fish: `fish_preexec`/`fish_postexec`; guarded so sourcing twice installs once and non-interactive shells skip
  - `type HookReader struct{ Dirs []string }`; `func (h *HookReader) Run(ctx context.Context, alive func(pid int) bool, out chan<- wire.Command, alerts chan<- wire.Alert)` — fsnotify on both dirs; on write: read new records, truncate file; file > 1 MiB → truncate + `Alert{Kind:"gap"}`; shell exited (`alive` false) → delete file
  - `EventID` = hex sha256 of `boot_id/shell_pid/starttime/seq` (seq = record ordinal within that shell)

- [ ] **Step 1: Write the failing tests**

```go
func TestHookRecord_Parse(t *testing.T)        // two records with \x1f fields and \0 ends, cmd containing newlines and \x1e → both parsed, cmd intact
func TestHooks_RealBash(t *testing.T)          // run `bash -i` with BashZshHook sourced, HOME tmp, dir tmp; type `cd /tmp; false; echo hi > f` → three commands; second exit 1; cwd /tmp
func TestHooks_TruncateAndGap(t *testing.T)    // 2 MiB file → one gap alert, file size 0 afterwards
func TestHooks_DeleteOnShellExit(t *testing.T) // alive(pid)=false → file removed
func TestInstallProfileScripts_IncludesHooks(t *testing.T) // devpod.sh contains "trap" and "PROMPT_COMMAND"; devpod.fish contains "fish_preexec"
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/ -run Hook ./cmd/supervisor/ -run InstallProfile` — Expected: FAIL.
- [ ] **Step 3: Implement.** `TestHooks_RealBash` skips when `bash` is absent.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: shell command hooks"`

---

### Task 10: container info and snapshots

**Files:**
- Create: `internal/telemetry/agent/info.go`, `info_test.go`

**Interfaces:**
- Produces:
  - `func CollectInfo(root string, cg *Cgroup, gpus []wire.GPU, rdma []RDMADev, collectors map[string]string /* name → "ok"|"disabled: …" */) wire.Info` — os-release, kernel (`/proc/sys/kernel/osrelease`), cgroup limits and cpuset, GPU list, RDMA devices, shell (`$DEVPOD_SHELL` or resolved login shell), `AgentVersion`
  - `func CollectSnapshot(root string, procs map[procfs.PID]*procfs.Proc, attrib map[procfs.PID]procfs.Attribution, pvcMounts []string) wire.Snapshot` — top 20 by CPU and by RSS (with attribution), process and thread counts, zombies, LISTEN ports (`/proc/net/tcp{,6}` + owning pid), `statfs` of each PVC mount point
  - PVC mount points: entries of `/proc/self/mountinfo` whose source is a block device or NFS and whose mount point is not `/`, `/etc/*`, `/opt/devpod`, or a hostPath home

- [ ] **Step 1: Write the failing tests**

```go
func TestInfo_OSReleaseKernelLimits(t *testing.T) // fixture → PrettyName "Ubuntu 24.04", Kernel "6.8.0", CPULimit 8
func TestSnapshot_ListenPortsAndTop(t *testing.T) // pid 30 owns LISTEN inode on :8888 → port 8888 pid 30; top lists capped at 20
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/ -run 'Info|Snapshot'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: container info and snapshots"`

---

### Task 11: priority buffer and uploader

**Files:**
- Create: `internal/telemetry/agent/buffer.go`, `uploader.go`, `buffer_test.go`, `uploader_test.go`

**Interfaces:**
- Consumes: `wire.Encode`, `wire.Ack`, `Kind.Audit()` (Task 1).
- Produces:
  - `type Buffer struct{...}`; `func NewBuffer(total int64) *Buffer` — audit partition `total/4`, metrics partition the rest
  - `func (b *Buffer) Add(seq uint64, recs []wire.Record)` — splits by `Audit()`; metrics overflow drops the oldest metrics records; audit overflow drops the oldest audit records and queues one `Alert{Kind:"gap", Detail:"<n> audit records dropped"}` into the audit partition
  - `func (b *Buffer) Pending(maxBytes int) (from, to uint64, recs []wire.Record)`; `func (b *Buffer) Ack(seq uint64)`
  - `type Uploader struct{ URL, TokenPath string; Client *http.Client }`; `func (u *Uploader) Run(ctx context.Context, h func() wire.Header, b *Buffer)` — one request in flight; re-reads the token file before each request; body ≤ 3.5 MiB compressed; on 2xx reads `Ack` and calls `b.Ack`; on 503/5xx/network error backs off 1 s → 30 s with jitter; on 401 logs once per minute and backs off 60 s

- [ ] **Step 1: Write the failing tests**

```go
func TestBuffer_MetricsDroppedBeforeAudit(t *testing.T) // fill past total with metrics then audit → all audit records still pending, oldest metrics gone
func TestBuffer_AuditOverflowEmitsGap(t *testing.T)     // audit partition overflow → exactly one gap alert carrying the dropped count
func TestUploader_AckAndRetry(t *testing.T)            // httptest server: 503 twice, then 200 {"acked_seq":3} → buffer empty up to 3; retries spaced ≥ 1s (fake clock)
func TestUploader_TokenReread(t *testing.T)            // token file rewritten between requests → second request carries the new Authorization: Bearer token
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/ -run 'Buffer|Uploader'` — Expected: FAIL.
- [ ] **Step 3: Implement** with an injectable clock.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: priority buffer and uploader"`

---

### Task 12: agent run loop

**Files:**
- Create: `internal/telemetry/agent/agent.go`, `agent_test.go`, `agent_bench_test.go`
- Modify: `cmd/supervisor/telemetry.go` (call `agent.Run`)

**Interfaces:**
- Consumes: everything in Tasks 2–11.
- Produces: `type Env struct{ ProcRoot, SysRoot, DevRoot, CgroupRoot string; SSHDPid func() int; Now func() time.Time; GPU GPUCollector; TCP procfs.TCPStats }`; `func Run(ctx context.Context, cfg Config, env Env) error`
  - every `ProcScanInterval`: scan + attribute (when `ProcessTracking`)
  - every `MetricsInterval`: one batch with `Container`, `GPU`, `RDMA`, active `Proc` rows (CPU/IO/net > 0 or RSS changed) plus top `TopN` per metric, everything else summed into one `Proc` row with `Pid 0` ("其他", §6.2), `ProcEvent`s, `SSHConn`s, hook `Command`s
  - every 5 s: `FullProcs`; every 15 s: `Snapshot`; at start and every 60 s when changed: `Info`; every `SocketMapInterval`: rebuild the socket map
  - `BootID`: ULID created at agent start; `Seq` starts at 1
  - each collector runs under `recover`; a collector that panics three times is disabled and reported as `"disabled: <err>"` in `Info`
  - `cfg` is re-read every 30 s; a changed `IngestURL` takes effect on the next request

- [ ] **Step 1: Write the failing tests**

```go
func TestAgent_OneSecondBatch(t *testing.T)        // fake roots + fake clock: after 3 ticks the uploader server saw seq 1..3, each with a Container record, active Proc rows, and one Pid-0 row whose CPU equals the idle processes' sum
func TestAgent_CollectorPanicDisabled(t *testing.T)// GPU collector panicking → Info lists gpu "disabled: …", other records keep flowing
func BenchmarkAgent_300Processes(b *testing.B)     // fake /proc with 300 processes: one scan+batch per op; reports ns/op and allocs
```

- [ ] **Step 2: Run** `go test ./internal/telemetry/agent/ -run Agent_` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race ./internal/telemetry/... ./cmd/supervisor/` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "telemetry: agent run loop"`

---

### Task 13: controller hands out telemetry config

**Files:**
- Create: `internal/render/telemetry.go`, `internal/render/telemetry_test.go`
- Modify: `internal/controllers/devpod_controller.go` (`ensureHostKeySecret`), `internal/render/networkpolicy.go`, `cmd/controller/main.go`, `internal/controllers/devpod_controller_test.go`

**Interfaces:**
- Consumes: `wire.Token` (Task 1), `agent.Config` json shape (Task 2).
- Produces:
  - `type TelemetrySettings struct{ IngestURL string; Key []byte; Defaults agent.Config }` on `DevPodReconciler.Telemetry *TelemetrySettings` (nil = telemetry off)
  - `func TelemetrySecretData(dp *devpodv1alpha1.DevPod, s *TelemetrySettings) (map[string][]byte, error)` → keys `ingest-token`, `telemetry.json` (`hostNetwork` from the DevPod's pod spec)
  - `ensureHostKeySecret` patches exactly these two keys when they differ (also on existing Secrets); never touches the SSH keys; Pod spec and spec-hash unchanged
  - controller flags `--ingest-url` (default `http://devpod-webui-ingest.devpod-system.svc:8081/ingest/v1/batch`), `--ingest-key-file` (empty = off), `--telemetry-metrics-interval`, `--telemetry-proc-scan-interval`, `--telemetry-topn`, `--telemetry-gpu`, `--telemetry-rdma`, `--telemetry-hooks`
  - `OwnerAllowNetworkPolicy` gains an egress rule to pods `app.kubernetes.io/name: devpod-webui` in the gateway namespace on TCP 8081 when telemetry is on

- [ ] **Step 1: Write the failing tests**

```go
func TestTelemetrySecretData_TokenAndHostNetwork(t *testing.T) // token == wire.Token(key, uid); hostNetwork pod → "hostNetwork":true
func TestDevPodReconciler_TelemetryKeysReconciled(t *testing.T) // envtest: existing Secret without the keys gets them; changing --telemetry-topn updates telemetry.json; Pod spec-hash annotation unchanged
func TestDevPodReconciler_TelemetryOffLeavesSecret(t *testing.T) // Telemetry nil → Secret keys untouched
func TestOwnerAllowNetPol_IngestEgress(t *testing.T)           // rule to devpod-webui :8081 present only when enabled
```

- [ ] **Step 2: Run** `go test ./internal/render/ -run Telemetry && KUBEBUILDER_ASSETS=… go test ./internal/controllers/ -run Telemetry` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `bash hack/test.sh -run 'Telemetry|OwnerAllow'` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "controller: per-DevPod ingest token and telemetry config"`

---

### Task 14: ClickHouse schema and writer

**Files:**
- Create: `internal/audit/store/migrations/0002_telemetry.sql`, `internal/audit/store/telemetry.go`, `internal/audit/store/telemetry_integration_test.go`
- Modify: `internal/audit/store/migrate.go` (retention parameters), `cmd/webui/audit.go` (`migrate` subcommand flags)

**Interfaces:**
- Produces:
  - Tables per spec §8.2 container-side: `ssh_conns`, `processes` (ReplacingMergeTree(version)), `container_info` (ReplacingMergeTree(version)), `container_snapshots`, `metrics_container_1s`, `metrics_gpu_1s`, `metrics_rdma_1s`, `metrics_process_1s`; the four `*_1s` metrics tables are **ReplacingMergeTree** ordered by `(devpod_uid, ts[, gpu_uuid | ibdev, port | pid, starttime])` so a replayed batch collapses; partition `toDate(ts)`
  - `metrics_container_1m`, `metrics_gpu_1m`, `metrics_rdma_1m`: AggregatingMergeTree with `avgState/maxState/quantileState(0.95)` fed by materialized views from the 1 s tables
  - `type Retention struct{ AuditDays, Metrics1sDays, Metrics1mDays, Process1sHours, SnapshotDays int }`; `func Migrate(ctx, c *CH, r Retention) error` (existing callers pass `Retention{AuditDays: n, Metrics1sDays: 7, Metrics1mDays: 90, Process1sHours: 48, SnapshotDays: 7}`)
  - `type TelemetryBatch struct{ Containers []ContainerMetricRow; GPUs []GPUMetricRow; RDMAs []RDMAMetricRow; Procs []ProcMetricRow; ProcRows []ProcessRow; SSHConns []SSHConnRow; Commands []CommandRow; Infos []ContainerInfoRow; Snapshots []SnapshotRow; Alerts []AlertRow }`
  - `func (c *CH) InsertTelemetry(ctx context.Context, b TelemetryBatch, token string) error` — one insert per non-empty table with `insert_deduplication_token = token + "/" + table`
  - webui `migrate` flags: `--metrics-retention-1s-days 7`, `--metrics-retention-1m-days 90`, `--process-retention-hours 48`

- [ ] **Step 1: Write the failing tests** (integration, `-tags integration`)

```go
func TestTelemetry_InsertAndReplayCollapses(t *testing.T) // insert one batch twice with different tokens → SELECT count() FROM metrics_container_1s FINAL == 1
func TestTelemetry_OneMinuteRollup(t *testing.T)          // 60 rows cpu 1..60 within one minute → avgMerge == 30.5 in metrics_container_1m
func TestMigrate_TelemetryTTLs(t *testing.T)              // Retention{Metrics1sDays: 3} → create_table_query of metrics_container_1s contains toIntervalDay(3)
```

- [ ] **Step 2: Run** `KUBE_CONTEXT=zjusct bash hack/test-integration.sh -run 'Telemetry|Migrate_Telemetry'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same — Expected: PASS; then `KUBE_CONTEXT=zjusct bash hack/test-integration.sh` — Expected: all PASS.
- [ ] **Step 5: Commit** `git commit -m "audit/store: telemetry tables and writer"`

---

### Task 15: telemetry queries

**Files:**
- Create: `internal/audit/store/telemetry_query.go`; extend `internal/audit/store/telemetry_integration_test.go`

**Interfaces:**
- Produces (every query time-bounded; `TestReadQueries_UseDateIndex` from phase 1 is extended to cover them):
  - `func (c *CH) MetricsRange(ctx, uid string, from, to time.Time, step time.Duration) (*MetricSeries, error)` — ranges ≤ 1 h read the 1 s tables, longer ranges the 1 m tables; `MetricSeries` holds parallel arrays per series name (`cpu`, `cpu_limit`, `cpu_throttled`, `mem_anon`, `mem_file`, `mem_limit`, `io_r`, `io_w`, `net_rx`, `net_tx`, `net_tcp_rx`, `net_tcp_tx`, `gpu.<uuid>.{util,mem,power,temp}`, `rdma.<ibdev>/<port>.{rx,tx,cnp,retrans}`)
  - `func (c *CH) LatestInfo(ctx, uid string, since time.Time) (*ContainerInfoRow, *SnapshotRow, error)`
  - `func (c *CH) IdleCandidates(ctx, now time.Time, cpuPct, gpuPct float64) ([]string /* uid */, error)` — 24 h window from the 1 m tables, joined with "no session in 24 h" from `sessions`
  - `func (c *CH) PendingAttributions(ctx, since time.Time) ([]SSHConnRow, error)` and `func (c *CH) OpenSessionsByBackend(ctx, since time.Time) (map[string]map[string]SessionRef /* uid → backend_local_addr → {session_id,user} */, error)`
  - `func (c *CH) HookMissing(ctx, from, to time.Time) ([]SessionRow, error)` — sessions ≥ 60 s with pty input ≥ 1 KiB and no `shell_hook` commands
  - `func (c *CH) LastSessionByUser(ctx, since time.Time) (map[string]time.Time, error)`

- [ ] **Step 1: Write the failing tests** (integration)

```go
func TestMetricsRange_PicksTable(t *testing.T)     // 30 min range → 1s rows; 3 h range → 1m rows; both return the cpu series
func TestIdleCandidates(t *testing.T)              // uid A cpu 1%, no session 25 h → included; uid B cpu 40% → excluded
func TestHookMissing(t *testing.T)                 // 90 s pty session with 2 KiB in, zero shell_hook commands → returned
func TestReadQueries_UseDateIndex(t *testing.T)    // extended: every new query passes with force_index_by_date=1
```

- [ ] **Step 2: Run** `KUBE_CONTEXT=zjusct bash hack/test-integration.sh -run 'MetricsRange|Idle|HookMissing|UseDateIndex'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit/store: telemetry queries"`

---

### Task 16: webui leader election and ingest EndpointSlice

**Files:**
- Create: `internal/webui/leader/elector.go`, `endpointslice.go`, `leader_test.go`
- Modify: `cmd/webui/main.go` / create `cmd/webui/telemetry.go` (flags `--ingest-listen :8081`, `--ingest-key-file`, `--leader-election-namespace devpod-system`, `--ingest-service devpod-webui-ingest`; `POD_NAME`/`POD_IP` env)

**Interfaces:**
- Produces:
  - `type Elector struct{ Client kubernetes.Interface; Namespace, Name, Identity string }`; `func (e *Elector) Run(ctx context.Context, onStart func(ctx context.Context), onStop func())` — client-go `leaderelection` with Lease lock `devpod-webui-leader`, durations from Global Constraints
  - `func (e *Elector) IsLeader() bool`
  - `func PublishIngestEndpoint(ctx, c client.Client, ns, service, podName, podIP string, port int32) error` — creates/updates EndpointSlice `<service>-leader` (label `kubernetes.io/service-name: <service>`, managed-by `devpod-webui`) to exactly one endpoint
  - `func LeaderAddr(ctx, c client.Client, ns, service string) (string, error)` — the published `ip:port` (followers use it for `/internal/live`)

- [ ] **Step 1: Write the failing tests** (envtest)

```go
func TestElector_SingleLeaderAndHandover(t *testing.T) // two electors → exactly one onStart; cancel the leader's ctx → the other's onStart within 20s; the old one's IsLeader() false before the new onStart
func TestPublishIngestEndpoint(t *testing.T)           // slice has one endpoint 10.1.2.3:8081; republish with a new IP replaces it; LeaderAddr returns the new one
```

- [ ] **Step 2: Run** `KUBEBUILDER_ASSETS=… go test ./internal/webui/leader/` — Expected: FAIL.
- [ ] **Step 3: Implement.** `onStop` must run before the lease can be acquired elsewhere (ReleaseOnCancel = true).
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: leader election and ingest endpoint"`

---

### Task 17: live state and rollups

**Files:**
- Create: `internal/webui/live/state.go`, `rollup.go`, `snapshot.go`, `live_test.go`

**Interfaces:**
- Produces:
  - `type State struct{...}`; `func NewState(now func() time.Time) *State`; `func (s *State) Apply(uid string, recs []wire.Record)` — keeps the latest `Container`, `GPU[]`, `RDMA[]`, active `Proc[]`, latest `FullProcs`, live `SSHConn` set with resolved users; staleness 5 s
  - `type Inputs struct{ DevPods []DevPodRef /* uid,name,owner,node,phase,limits,gpuRequest,createdAt */; Nodes []NodeRef /* name,ready,allocatable cpu/mem/gpu,labels */; Quotas map[string]Quota; Idle map[string]bool; Alerts []AlertRef; Presence map[string][]string }`
  - `type Snapshot struct{ TS time.Time; DevPods map[string]DevPodLive; Nodes []NodeLive; Users []UserLive; KPI KPI; Attention []AttentionItem }` — JSON field names exactly as UI spec §6.1 (`cpu, cpuLimit, mem, memLimit, gpu[{util,memUsed,memTotal}], netRx, netTx, netKind, rdmaRx, rdmaTx, rdmaKind, ioR, ioW, sessions[{user}]`)
  - `func (s *State) Snapshot(in Inputs) Snapshot`; `func (s *State) Processes(uid string, all bool) []ProcLive`
  - Rollups: node CPU used = Σ DevPod cpu on the node; node allocated = Σ DevPod pod requests on the node (DevPod pods only); GPU used per node = Σ DevPod GPU requests, occupants from reported GPU UUIDs; users = Σ per owner; KPI totals over Ready nodes
  - Attention kinds (phase 2): `idle_gpu` (Idle[uid] and GPU > 0), `pending_long` (Pending > 10 min), `alert` (alerts in the last hour), `quota_high` (any of cpu/mem/gpu usage ≥ 90 % of quota); sorted by kind priority then age

- [ ] **Step 1: Write the failing tests**

```go
func TestState_StaleSampleIsNoData(t *testing.T)   // sample at t0, Snapshot at t0+6s → DevPod present with Cpu == nil (renders "—")
func TestRollup_NodeAndUser(t *testing.T)          // two DevPods on g206 (2.0 and 3.5 cores, 1 and 2 GPUs) → node cpu 5.5, gpuUsed 3; owner totals summed
func TestRollup_Attention(t *testing.T)            // idle uid with GPU → idle_gpu; Pending 11 min → pending_long; usage 95% of quota → quota_high
func TestSnapshot_JSONFieldNames(t *testing.T)     // marshalled DevPodLive has exactly the UI spec §6.1 keys
```

- [ ] **Step 2: Run** `go test ./internal/webui/live/` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: live state and rollups"`

---

### Task 18: ingest handler and merging writer

**Files:**
- Create: `internal/webui/ingest/handler.go`, `writer.go`, `resolve.go`, `ingest_test.go`

**Interfaces:**
- Consumes: `wire.Decode`, `wire.Token`, `wire.Ack` (Task 1); `store.TelemetryBatch`, `InsertTelemetry`, `OpenSessionsByBackend` (Tasks 14–15); `Elector.IsLeader` (Task 16); `live.State.Apply` (Task 17).
- Produces:
  - `type Handler struct{ Key []byte; IsLeader func() bool; Writer *Writer; State *live.State; DevPods func(uid string) (owner, name string, ok bool) }` serving `POST /ingest/v1/batch`: 503 when not leader; 413 above 4 MiB; 401 when the bearer token ≠ `wire.Token(Key, header.DevPodUID)` or the uid is unknown; 429 above 10 requests/s per uid (token bucket, burst 20)
  - per `(uid, boot_id)` high-water mark: records with `seq ≤ written` are acknowledged without rewriting
  - `type Writer struct{...}`; `func (w *Writer) Submit(ctx, rows store.TelemetryBatch, done func(error))` — merges everything submitted within one second into one `InsertTelemetry` call with token = sha256 of the sorted `uid/boot/seq` list; `done` fires after the insert; the handler replies `Ack{AckedSeq}` only after `done(nil)`, 503 on error
  - `type Resolver struct{...}` — `ssh_conn.peer_addr` → `(session_id, user)` from `OpenSessionsByBackend` refreshed every 5 s; unresolved → `session_id = "pending"`
  - records of kind `alert` and attributions `untracked_entry`/`unattributed` become `alerts` rows (`source = supervisor`) and a k8s Event on the DevPod (reason `TelemetryAlert`, rate-limited to one per kind per DevPod per minute)

- [ ] **Step 1: Write the failing tests**

```go
func TestIngest_NotLeader503(t *testing.T)
func TestIngest_BadToken401AndOversize413(t *testing.T)
func TestIngest_AckAfterWrite(t *testing.T)                 // fake store blocks 500ms → response not sent before the insert returns; insert error → 503
func TestIngest_MergesOneInsertPerSecond(t *testing.T)      // 50 DevPods post within one second → fake store saw 1 InsertTelemetry with 50 container rows
func TestIngest_FailoverResendNoLossNoDup(t *testing.T)     // leader A acks seq 1..5, loses leadership mid-seq 6 (503); agent resends 6..8 to leader B → store has 1..8 exactly once (B dedups nothing, A wrote nothing for 6)
func TestIngest_ReplayAcknowledgedWithoutRewrite(t *testing.T) // same seq posted twice to one leader → one insert
func TestResolver_PendingThenResolved(t *testing.T)         // peer addr unknown → "pending"; after the session row appears → session id
```

- [ ] **Step 2: Run** `go test -race ./internal/webui/ingest/` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: telemetry ingest on the leader"`

---

### Task 19: live distribution and browser streams

**Files:**
- Create: `internal/webui/live/hub.go`, `follower.go`, `internal/webui/api_live.go`, `internal/webui/api_live_test.go`
- Modify: `internal/webui/watch_stream.go` (new `Kind` values `metrics`, `processes`), `internal/webui/server.go` (routes), `cmd/webui/telemetry.go` (wiring)

**Interfaces:**
- Consumes: `State.Snapshot`, `State.Processes` (Task 17), `LeaderAddr` (Task 16).
- Produces:
  - `type Hub struct{...}`; `func (h *Hub) Publish(s Snapshot)`; `func (h *Hub) Subscribe() (<-chan Snapshot, func())`; `func (h *Hub) Subscribers() int`
  - leader: a 1 s ticker builds `Snapshot` only when `Subscribers() > 0` or a follower is connected, publishes locally, and serves `GET /internal/live` on :8081 (SSE, token `wire.Token(Key, "internal-live")`) carrying the snapshot plus per-DevPod processes
  - follower: `func Follow(ctx, addr func() (string, error), token string, h *Hub)` — reconnects with backoff 1 s → 5 s, re-resolving the leader address each time; while disconnected publishes nothing (browsers show the reconnect bar)
  - `GET /api/live/devpods?scope=mine|all` (SSE; `all` admin-only, 403 otherwise) — each event `{ts, items:[{name, owner, …DevPodLive}]}` filtered to visible DevPods
  - `GET /api/admin/live/overview` (SSE, admin-only) — `{ts, kpi, nodes, users, attention}`
  - per-DevPod stream: `metrics` events (full sample incl. per-GPU, per-RDMA) and `processes` events (top 10 by CPU, or the full snapshot when `?procs=all`); non-admin viewers get `cmdline` replaced by `basename(exe)` for processes attributed to another user's session
  - SSE event `liveStatus {connected:bool}` when the follower link drops/returns

- [ ] **Step 1: Write the failing tests**

```go
func TestLiveDevPods_ScopeAndVisibility(t *testing.T)   // bob sees only his DevPods; scope=all as bob → 403; as admin → all
func TestOverview_AdminOnly(t *testing.T)
func TestProcesses_RedactOtherUsersCmdline(t *testing.T) // owner alice, process attributed to collaborator carol → alice sees exe basename; admin sees full cmdline
func TestFollower_RelaysAndReconnects(t *testing.T)     // fake leader serves 3 snapshots then dies → follower hub got 3, liveStatus false, reconnects to the new address
func TestHub_NoWorkWithoutSubscribers(t *testing.T)     // leader ticker builds no Snapshot when Subscribers()==0 and no follower
```

- [ ] **Step 2: Run** `KUBEBUILDER_ASSETS=… go test ./internal/webui/ -run 'Live|Overview|Processes_' ./internal/webui/live/` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: live streams for the console UI"`

---

### Task 20: history, info, nodes and user usage APIs

**Files:**
- Create: `internal/webui/api_metrics.go`, `internal/webui/api_nodes.go`, tests alongside
- Modify: `internal/webui/api_users.go` (usage fields), `internal/webui/server.go`, `deploy/chart/templates/webui-rbac.yaml` (nodes get/list/watch)

**Interfaces:**
- Consumes: `MetricsRange`, `LatestInfo`, `LastSessionByUser` (Task 15); `State` (Task 17); existing Kore topology reader.
- Produces:
  - `GET /api/devpods/{name}/metrics?range=15m|1h|24h|7d&step=` → `MetricSeries` JSON; owner or admin
  - `GET /api/devpods/{name}/info` → `{info, snapshot}` or 404 `{"code":"NO_TELEMETRY"}` (UI shows "监控在 Pod 下次重建后生效")
  - `GET /api/devpods/{name}/processes` → `State.Processes(uid, true)` with the same redaction as the stream (spec §9.3)
  - `GET /api/admin/nodes` → `[]NodeView{name, ready, labels, cpu{allocatable, allocated, used}, mem{…}, gpu{model, total, used, cards[{index, uuid, devpod, owner, util}]}, rdma[], devpods}`; `GET /api/admin/nodes/{n}` adds `devpods[]` and Kore `topology`
  - `GET /api/admin/users` items gain `usage{cpu, mem, gpu, devpodsRunning}` and `lastSession`
  - ClickHouse unavailable → nodes and users still return (usage from live state, `lastSession` omitted); metrics/info → 503 `AUDIT_UNAVAILABLE`

- [ ] **Step 1: Write the failing tests**

```go
func TestMetricsAPI_OwnerOnly(t *testing.T)       // bob GET alice's metrics → 404; admin → 200
func TestInfoAPI_NoTelemetry(t *testing.T)        // no rows → 404 NO_TELEMETRY
func TestNodesAPI_Rollup(t *testing.T)            // envtest nodes + fake live state → cpu.used, gpu.cards occupants
func TestUsersAPI_UsageWithoutClickHouse(t *testing.T) // store nil → usage present, lastSession absent, 200
```

- [ ] **Step 2: Run** `KUBEBUILDER_ASSETS=… go test ./internal/webui/ -run 'MetricsAPI|InfoAPI|NodesAPI|UsersAPI'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: metrics history, container info, nodes and user usage"`

---

### Task 21: leader minute jobs

**Files:**
- Create: `internal/webui/leader/jobs.go`, `jobs_test.go`

**Interfaces:**
- Consumes: `IdleCandidates`, `PendingAttributions`, `OpenSessionsByBackend`, `HookMissing` (Task 15); `InsertTelemetry`, `Insert` (alerts); k8s Events via the manager's recorder.
- Produces: `type Jobs struct{...}`; `func (j *Jobs) RunOnce(ctx, now time.Time) error` called every minute while leading:
  - **backfill** (§5.4): pending `ssh_conns` / `processes` / `commands` matched to sessions by `(devpod_uid, backend_local_addr)` and time overlap → re-insert with `version+1`
  - **cross-check** (§5.8): `hook_missing`, `agent_silent` (Running DevPod with `container_info` seen but no sample for > 3 min; once per incident) → `alerts` rows + k8s Event on the DevPod
  - **idle**: `IdleCandidates` with the Global Constraints thresholds → `live.Inputs.Idle`
  - flags `--idle-cpu-pct 5`, `--idle-gpu-pct 5`, `--agent-silent-after 3m`

- [ ] **Step 1: Write the failing tests**

```go
func TestJobs_BackfillReinsertsHigherVersion(t *testing.T) // pending ssh_conn + matching session → one ssh_conns row version 2 with the session id, and its processes re-inserted
func TestJobs_AgentSilentOncePerIncident(t *testing.T)     // silent for 4 min across 3 runs → one alert; resumes then silent again → second alert
func TestJobs_StopWhenLeadershipLost(t *testing.T)         // ctx cancelled mid-run → returns promptly, no further inserts
```

- [ ] **Step 2: Run** `go test ./internal/webui/leader/ -run Jobs` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: leader backfill, cross-check and idle jobs"`

---

### Task 22: chart wiring

**Files:**
- Create: `deploy/chart/templates/webui-ingest.yaml` (selectorless Service `devpod-webui-ingest` :8081 + NetworkPolicy allowing `devpods`-namespace pods and webui peers to :8081), `deploy/chart/templates/ingest-key.yaml` (Secret `devpod-ingest-key`, 48 random bytes, preserved across upgrades with `lookup`)
- Modify: `deploy/chart/values.yaml` (`telemetry.enabled false`, `telemetry.*` intervals, `telemetry.retention.*`, idle thresholds), `deploy/chart/templates/webui.yaml` (port 8081, `POD_NAME`/`POD_IP` env, ingest/leader flags, key mount), `deploy/chart/templates/controller.yaml` (`--ingest-url`, `--ingest-key-file`, telemetry flags, key mount), `deploy/chart/templates/webui-rbac.yaml` (Lease in system ns; EndpointSlices get/list/watch/create/update in system ns; nodes get/list/watch), `deploy/chart/templates/audit-migrate.yaml` (retention flags), `hack/test-chart.sh`

**Interfaces:**
- Consumes: every flag named in Tasks 13, 14, 16, 21.
- Produces: `telemetry.enabled=true` requires `audit.enabled=true` (fail with a clear message otherwise).

- [ ] **Step 1: Add the failing chart assertions** to `hack/test-chart.sh`: render `telemetry` with `audit.enabled=true telemetry.enabled=true` and check `name: devpod-webui-ingest`, no `selector:` in that Service, `containerPort: 8081`, `--ingest-key-file=`, `--leader-election-namespace=devpod-system`, `resources: ["endpointslices"]`, `resources: ["leases"]`, controller `--ingest-url=http://devpod-webui-ingest.devpod-system.svc:8081/ingest/v1/batch`, migrate `--metrics-retention-1s-days=7`; `telemetry.enabled=true` without audit fails mentioning `audit.enabled`; defaults render none of it.
- [ ] **Step 2: Run** `bash hack/test-chart.sh` — Expected: FAIL on the new assertions.
- [ ] **Step 3: Implement** the templates.
- [ ] **Step 4: Run** `bash hack/test-chart.sh` — Expected: `chart: OK`.
- [ ] **Step 5: Commit** `git commit -m "chart: telemetry wiring"`

---

### Task 23: isolated end-to-end on zjusct

**Files:**
- Create: `hack/e2e-telemetry.sh`
- Modify: `hack/e2e-audit.sh` (shared helpers extracted to `hack/e2e-lib.sh` if it shortens both)

**Interfaces:**
- Consumes: binaries from Tasks 1–21; storage pods from `hack/integration/fixtures.yaml`.
- Produces: a script that, in `devpod-audit-it` only:
  1. starts the runner pod with gateway + webui (`--ingest-key-file`, leader election in `devpod-audit-it`, ingest Service `devpod-telemetry-e2e-ingest`),
  2. starts a box pod: init container from the published supervisor image copies `/opt/devpod`; main container `debian:trixie-slim` (procps, bash, tmux) waits for `/opt/devpod/.ready` then `exec /opt/devpod/devpod-supervisor`; the script `kubectl cp`s the freshly built supervisor binary in and touches `.ready`; a test Secret mounted at `/etc/devpod` carries host key, `authorized_keys`, `ingest-token`, `telemetry.json`,
  3. writes the test DevPod status (endpoint = box IP) as in `hack/e2e-audit.sh`,
  4. SSHes through the gateway: `tmux new -d 'sleep 600'`, `cd /tmp && false`, a CPU burner for 5 s,
  5. asserts:
     - ClickHouse has `metrics_container_1s` rows for the box;
     - `commands` has `source='shell_hook'` rows for `cd /tmp` and `false` (exit 1);
     - the `sleep 600` process has the SSH session's `session_id` (directly or after one backfill minute);
     - `kubectl exec` into the box produces an `untracked_entry` alert;
     - `GET /api/live/devpods` shows the burner's CPU > 0.5 within 3 s;
     - deleting the leader webui pod → the second replica takes over and samples resume within 20 s, with no seq gap in `metrics_container_1s`,
  6. cleans up unless `E2E_KEEP=1`.

- [ ] **Step 1: Write the script** with the assertions above as `die`-guarded checks.
- [ ] **Step 2: Run** `KUBE_CONTEXT=zjusct bash hack/e2e-telemetry.sh` — Expected: `e2e-telemetry: OK`.
- [ ] **Step 3: Commit** `git commit -m "e2e: isolated telemetry end-to-end"`

---

### Task 24: measurements (spec §14 items 0, 4–7)

**Files:**
- Create: `hack/telemetry-load/main.go`, `docs/superpowers/notes/2026-11-telemetry-phase2-measurements.md`
- Modify: spec §14 and §6.4 with the findings

**Interfaces:**
- Consumes: wire protocol (Task 1), runner setup from Task 23.
- Produces: `telemetry-load -n 400 -url … -key-file …` — N fake supervisors (one goroutine each) posting realistic 1 s batches (20 processes, 2 GPUs, 1 RDMA device).

- [ ] **Step 1: Item 0** — on zjusct (`devpod-audit-it`, two webui replicas), run `telemetry-load -n 400` for 10 min. Record:
  - leader CPU and RSS;
  - p50/p99 of the per-second merged insert;
  - ingest 503/429 counts;
  - browser-side latency, i.e. `ts` of the sample vs. arrival on `/api/live/devpods` (target < 1 s);
  - then delete the leader pod and record the time until samples resume and the count of missing seqs (target 0).
- [ ] **Step 2: Item 4** — in the e2e box, start 300 sleeping processes plus 5 busy ones; read the telemetry child's CPU (`/proc/<pid>/stat` deltas over 5 min) and RSS. Target < 2 % of one core, RSS < 30 MB; if over, tune intervals and record.
- [ ] **Step 3: Items 6 and 7 (need the human partner)** — GPU node: `nvidia-smi -lms 1000` stability over 1 h and MIG/time-slicing output. RDMA node: whether RoCE traffic also appears in the NIC counters. Both need a test pod on a GPU/RDMA node of zjusct; ask before scheduling it; otherwise record "pending, needs a GPU/RDMA node".
- [ ] **Step 4: Write the notes** and update the spec.
- [ ] **Step 5: Commit** `git commit -m "docs: telemetry phase-2 measurements"`
