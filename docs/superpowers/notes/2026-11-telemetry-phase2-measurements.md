# Telemetry phase 2 — measurements (2026-11-10, zjusct)

Spec: `docs/superpowers/specs/2026-11-09-devpod-audit-telemetry-design.md`
§14 items 0 and 4–7. All runs in namespace `devpod-audit-it` with the
isolated setup of `hack/e2e-telemetry.sh` (gateway runner, two webui
replicas, a box pod running the real supervisor, ClickHouse + Silo
fixtures). Production (`default`, `devpods`) untouched.

## Item 4 — supervisor overhead (§6.8)

Box: 311 processes (300 `sleep`, 5 busy loops at nice 19), no SSH session,
on a 104-core node; the box's readiness probe connects to sshd every 3 s
(sshd forks a child each time: ~0.7 new pids/s, a stand-in for light
user activity). CPU = utime + stime of the `devpod-supervisor telemetry`
child from `/proc/<pid>/stat` over 300 s, in % of **one** core.

| Step | CPU | RSS |
|---|---|---|
| as implemented in Tasks 1–23 | 31 % | 24 MB |
| scans read `stat` only; exe/cmdline/environ/status once per process; `io` only when CPU moved | 5.5 % (profile) | |
| light scans between full scans; TCP dump every 5 s when slow | 3.3 % (profile) | |
| reused zstd codecs (were allocated per upload) | 3.4 % | 25 MB |
| child with `GOMAXPROCS=2` (Go used 104 Ps; each GC cycle fanned out over them) + idle processes re-read every 5th scan | 2.29 % | 15 MB |
| fds walked only for processes that ran; session peer resolved once; attribution only on membership change; node-wide walks under a 0.1 % budget; leaner per-second records | 1.22 % | 14 MB |
| light scans probe only new pids (`/proc/loadavg` last pid) instead of listing `/proc` | 1.05 % | 16 MB |
| single `read` per `/proc` / cgroup file (no read-to-EOF, no fstat); idle processes re-read every 10th scan | **0.91 %** | **14 MB** |

Final: 0.43 % user + 0.48 % kernel; 67 read syscalls/s. Syscalls on this
node cost ~24 µs each (kernel time dominates), so the remaining lever is
fewer syscalls. Spec target was < 2 %; the user set < 1 % (met).

Where the cost went, and why:

- **`/proc` per process.** Every 500 ms the first version opened eight
  files per process via `os.ReadFile` (open, fstat, read, read-to-EOF,
  close). Now: `/proc/<pid>/stat` once per second for processes that ran
  (it carries ppid, sid, tty, state, threads, utime/stime, rss, start
  time), every 10 s for idle ones; static fields once per process (again
  after an exec, detected by a new `comm`); `io` only when CPU time moved.
  Rates divide by the time between two readings, so a process re-read
  after 10 s shows its average, not a spike.
- **Light scans.** Starts and exits are still caught at 500 ms / 1 s:
  between full scans the agent reads `/proc/loadavg`, whose last field is
  the newest pid in the container's pid namespace; unchanged → nothing to
  do, changed → probe just the new pids (threads skipped by `Tgid`).
  Listing `/proc` makes the kernel build a dentry per pid.
- **Node-wide kernel walks.** A sock_diag TCP dump (per-process TCP bytes)
  and `/proc/net/tcp` (listen ports, session peers) walk the node's whole
  TCP hash table, ~18 ms each on this node regardless of the container.
  They now run at most at a 0.1 %-of-a-core budget (≈ every 18 s here,
  every second on small nodes); per-process TCP rates are averages over
  that interval. Container-level network stays 1 s (`/proc/net/dev`),
  **except for hostNetwork DevPods**, whose container network *is* the
  TCP sum (net/dev would show the host): on large nodes it is an ~18 s
  average too. Session peers are
  read once per session — before, every scan re-read them while anyone
  was connected (not exercised by this run).
- **GC.** With no CPU limit Go set GOMAXPROCS = 104; each GC of a 6 MB
  heap cost ~40 ms of CPU. The child now runs with `GOMAXPROCS=2`.

Tools: `hack/agent-profile` (agent with a CPU/alloc profile inside a
container), the commands in this file.

## Item 0 — 400 supervisors

`hack/telemetry-load -n 400`: 400 fake supervisors (the agent's own
Buffer and Uploader) in the runner pod, each posting a realistic second
(container, 2 GPUs, 1 RDMA port, 20 active processes, the 60-process list
every 5 s); 20 of them carry their sample time so a watcher on the
*follower's* `/api/live/devpods` measures sample → browser latency. 400
DevPod objects in the test namespace back the ingest auth. Two webui
replicas as in the chart (`GOGC=200`, `GOMEMLIMIT=921MiB`). 10 minutes,
the leader pod deleted at minute 7.

| | Result |
|---|---|
| leader CPU | **0.72 cores** (≈ 1.8 mcores per DevPod) |
| leader RSS | 255–280 MB |
| merged writes | ~9,600 rows/s, ~52 flushes/min (1 s window + insert) |
| insert latency | p50 140 ms, p99 160–275 ms, 0 errors |
| ingest | 206,610 batches 200; no 503/429 while a leader existed |
| live latency | **p50 1.12 s, p99 1.80 s** (10,345 samples); max 9.2 s across the failover |
| failover | lease after 3 s; agents cut off 9.3 s median / 10.6 s max (kube-proxy picks up the new EndpointSlice, plus the agents' 1-2-4 s backoff); **0 records lost** (all buffered seqs acked after the switch) |

Leader CPU went from 3.05 cores (first run) to 0.72: DevPod lookup by a
UID index instead of listing every DevPod per batch (→ 1.46), then a
30 s auth cache, one JSON pass per NDJSON line (the json.Decoder stream
scanned every value twice) and `GOGC=200`. The chart now gives the
webui 500m / 2 CPU and 512 Mi / 1 Gi when telemetry is on (the default
500m limit could not carry 400 DevPods).

Live latency misses the < 1 s target at p50. Two halves: a sample waits
for its agent's previous upload to be acked (after the merged ClickHouse
write, ~0.6 s), then for the leader's 1 s snapshot tick (0.5 s on
average). A 500 ms tick would bring p50 to ~0.85 s at twice the snapshot
work and SSE traffic; not done (the spec says the UI refreshes every
second) — open for decision.

A single timed fake showed p50 1.31 s with p99 ≈ p50: its 1 s sampling
was phase-locked to the tick. Hence 20 timed fakes with random phases.

## Item 5 — 200 supervisors

Covered by item 0 at twice the load (ingest and ClickHouse kept up with
no 503/429 and no insert errors).

## e2e (`hack/e2e-telemetry.sh`)

Passes: hooks (`cd /tmp`, `false` exit 1), the CPU burner visible on
`/api/live/devpods` 1.0–2.6 s after it starts, `kubectl exec` →
`untracked_entry`, `sleep 600` under tmux attributed to the SSH session,
leader failover (lease 3 s, samples written again after 6 s, 40/40
seconds present). Found and fixed along the way: the webui exited on
SIGTERM before releasing its lease (19 s stalls on every rollout).

## Items 6 and 7 — GPU / RDMA nodes

Pending: they need a test pod on a GPU node and an RDMA node of zjusct;
the user chose to defer them (2026-11-10).
