// Mock API for the Playwright e2e: serves web/dist and a
// fake /api with 400 DevPods, live streams that tick every second, nodes,
// users and batch actions. Log in with alice/pw or admin/pw.
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const dist = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../dist");
const PORT = Number(process.env.PORT ?? 8080);
const GiB = 1024 ** 3;
const iso = (ms = 0) => new Date(Date.now() - ms).toISOString();

const phaseOf = (i) => (i % 10 === 7 ? "Stopped" : i % 23 === 5 ? "Failed" : i % 31 === 3 ? "Pending" : "Running");
const devpods = Array.from({ length: 400 }, (_, i) => {
  const owner = i < 3 ? "alice" : `user${String(i % 40).padStart(2, "0")}`;
  const name = i < 3 ? `alice-${["train", "notebook", "build"][i]}` : `${owner}-pod${i}`;
  const phase = i < 3 ? "Running" : phaseOf(i);
  const gpu = i % 5 === 0 ? { "nvidia.com/gpu": "2" } : {};
  return {
    metadata: { name, uid: `uid-${i}` },
    spec: {
      owner,
      running: phase !== "Stopped",
      persistence: i % 4 === 0 ? { size: "50Gi" } : undefined,
      pod: {
        spec: {
          containers: [
            {
              name: "dev",
              image: i % 3 ? "ubuntu:24.04" : "harbor.example.com/pytorch/pytorch:2.6.0-cuda12.4-cudnn9-devel-with-an-unusually-long-tag",
              resources: { limits: { cpu: "8", memory: "32Gi", ...gpu } },
            },
          ],
        },
      },
    },
    status: {
      phase,
      message: phase === "Failed" ? "ImagePullBackOff: harbor.example.com/missing:latest" : phase === "Pending" ? "拉取镜像（38%）" : undefined,
      hibernatedAt: phase === "Stopped" ? iso(3 * 86400e3) : undefined,
      retryCount: phase === "Failed" ? 2 : undefined,
      lastFailureAt: phase === "Failed" ? iso(5000) : undefined,
    },
  };
});
const podInfo = Object.fromEntries(
  devpods.filter((d) => d.status.phase === "Running").map((d, i) => [d.metadata.name, { node: `g${200 + (i % 12)}`, ip: `10.0.${i % 250}.${(i * 7) % 250}`, startedAt: iso(3600e3 * ((i % 30) + 1)) }]),
);

const t = () => Date.now() / 1000;
function live(d, i) {
  const running = d.status.phase === "Running";
  const wave = Math.sin(t() / 3 + i) * 0.5 + 0.5;
  const gpus = d.spec.pod.spec.containers[0].resources.limits["nvidia.com/gpu"] ? 2 : 0;
  return {
    name: d.metadata.name,
    owner: d.spec.owner,
    cpu: running ? +(1 + 6 * wave).toFixed(2) : null,
    cpuLimit: 8,
    mem: running ? Math.round((8 + 16 * wave) * GiB) : null,
    memLimit: 32 * GiB,
    gpu: running ? Array.from({ length: gpus }, (_, k) => ({ index: k, uuid: `GPU-${i}-${k}`, name: "A100", util: Math.round(100 * wave), memUsed: 20000, memTotal: 40960 })) : [],
    netRx: running ? Math.round(1e6 * wave) : null,
    netTx: running ? Math.round(3e5 * wave) : null,
    netKind: "pod",
    rdmaRx: null,
    rdmaTx: null,
    rdmaKind: "",
    ioR: running ? 1e5 : null,
    ioW: running ? 5e4 : null,
    sessions: running && i % 3 === 0 ? [{ user: d.spec.owner }] : [],
    idle: i % 17 === 0,
  };
}

const nodes = Array.from({ length: 400 }, (_, i) => ({
  name: `g${200 + i}`,
  ready: i !== 7,
  labels: { "nvidia.com/gpu.product": i % 2 ? "NVIDIA-A100" : "NVIDIA-H100", "kubernetes.io/arch": "amd64" },
  cpu: { allocatable: 128, allocated: 32 + i * 4 },
  mem: { allocatable: 1024 * GiB, allocated: (128 + i * 16) * GiB },
  gpu: { model: i % 2 ? "A100" : "H100", total: 8, used: i % 9, cards: [] },
  rdma: [{ resource: "rdma/hca", allocatable: 8 }],
  devpods: 20 + i,
}));
const users = Array.from({ length: 40 }, (_, i) => ({
  name: i === 0 ? "alice" : `user${String(i).padStart(2, "0")}`,
  displayName: i === 0 ? "Alice" : undefined,
  admin: false,
  hasPassword: true,
  devpods: 10,
  running: 8,
  usage: { cpu: String(8 + (i % 10)), memory: `${32 + i}Gi` },
  quota: i % 6 === 0 ? { compute: { cpu: "32" } } : undefined,
  source: i % 3 ? "password" : "ldap",
  customPolicy: i % 9 === 0,
  pubkeys: 1,
  lastSession: i % 7 ? iso(86400e3) : undefined,
}));

const me = (u) => ({
  user: u,
  admin: u === "admin",
  nameBudget: 30,
  quota: { maxDevPods: 10, compute: { cpu: "64", memory: "256Gi" } },
  usage: { devpods: 3, running: 3, compute: { cpu: "24", memory: "96Gi" }, storage: "0" },
  features: { pubkeySelfService: true, kore: false, passwordAuth: true, audit: true, telemetry: true, reservations: true, queue: true },
  hasPassword: true,
  ssh: { host: "gw.example.com", port: 2222 },
});

function json(res, code, body) {
  res.writeHead(code, { "Content-Type": "application/json" });
  res.end(body === undefined ? "" : JSON.stringify(body));
}

function sse(req, res, frame, every = 1000) {
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", Connection: "keep-alive" });
  res.write('event: liveStatus\ndata: {"connected":true}\n\n');
  const send = () => {
    const f = frame();
    for (const m of Array.isArray(f) ? f : [f]) if (m !== undefined) res.write(`data: ${JSON.stringify(m)}\n\n`);
  };
  send();
  const timer = every ? setInterval(send, every) : setInterval(() => res.write(": ping\n\n"), 15000);
  req.on("close", () => clearInterval(timer));
}

// the queue: bob's job running on two machines, carol's waiting for
// cards, one of alice's behind it; submitted ones are added
const jobs = [
  { id: "run01", title: "sft", owner: "bob", phase: "Running", nodes: 2, gpus: 8, cpus: 96, timeLimitSeconds: 43200, priority: 200, preemptible: true, submitted: iso(7200e3), startedAt: iso(3600e3),
    ranks: [{ rank: 0, node: "g200", devpod: "bob-run01-0", phase: "Running" }, { rank: 1, node: "g201", devpod: "bob-run01-1", phase: "Running" }] },
  { id: "wait1", title: "pretrain-with-a-rather-long-name", owner: "carol", phase: "Queued", nodes: 4, gpus: 8, cpus: 96, timeLimitSeconds: 86400, priority: 500, submitted: iso(1800e3),
    position: 1, reason: "capacity", detail: { need: "32", free: "18" }, expectedStart: new Date(Date.now() + 5 * 3600e3).toISOString(), ranks: [] },
  { id: "eval2", title: "eval", owner: "alice", phase: "Queued", nodes: 1, gpus: 4, cpus: 48, timeLimitSeconds: 3600, priority: 0, submitted: iso(600e3),
    position: 2, reason: "behind", detail: { head: "wait1" }, ranks: [], command: "python eval.py", workdir: "/home/alice" },
];
const jobView = (j, u) => ({ ...j, mine: j.owner === u, canManage: j.owner === u || u === "admin", ...(j.owner === u || u === "admin" ? {} : { command: undefined, workdir: undefined }) });
const userOf = (req) => /(?:^|;\s*)s=([a-z]+)/.exec(req.headers.cookie ?? "")?.[1];
const visible = (u, scope) => devpods.filter((d) => (scope === "all" && u === "admin") || d.spec.owner === u);

function samples(name) {
  const i = devpods.findIndex((d) => d.metadata.name === name);
  const l = live(devpods[i], i);
  return {
    ts: iso(),
    container: { ts: iso(), cpu: l.cpu ?? 0, cpu_limit: 8, throttled_usec: Math.round(t() * 1000), mem_ws: l.mem ?? 0, mem_anon: (l.mem ?? 0) * 0.7, mem_file: (l.mem ?? 0) * 0.3, mem_shmem: 0, mem_limit: 32 * GiB, oom_kills: 0, io_r: 1e5, io_w: 5e4, io_r_iops: 10, io_w_iops: 5, net_rx: l.netRx ?? 0, net_tx: l.netTx ?? 0, net_tcp_rx: (l.netRx ?? 0) * 0.9, net_tcp_tx: (l.netTx ?? 0) * 0.9, net_other_rx: 0, net_other_tx: 0, net_kind: "pod", pids: 120 },
    gpus: l.gpu.map((g) => ({ ts: iso(), index: g.index, uuid: g.uuid, name: g.name, util: g.util, mem_used: g.memUsed, mem_total: g.memTotal, power: 250, temp: 60 })),
    rdma: [],
  };
}
const procs = (n) =>
  Array.from({ length: n }, (_, k) => ({ pid: 100 + k, start: 1000 + k, exe: "/usr/bin/python3", cmdline: `python train.py --worker ${k}`, cpu: +(Math.random() * 2).toFixed(2), rss: (k + 1) * 50e6, ioR: 0, ioW: 0, tcpRx: 0, tcpTx: 0, user: "alice", how: "tree" }));

const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://localhost");
  const p = url.pathname;
  if (p === "/healthz") return json(res, 200, { ok: true });
  if (p === "/api/auth/config") return json(res, 200, { password: true, oauth: false });
  if (p === "/api/auth/password" && req.method === "POST") {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      const { username, password } = JSON.parse(body || "{}");
      if (password !== "pw" || !["alice", "admin"].includes(username)) return json(res, 401, { code: "UNAUTHORIZED", message: "用户名或密码错误" });
      res.setHeader("Set-Cookie", `s=${username}; Path=/; HttpOnly`);
      json(res, 200, {});
    });
    return;
  }
  if (p === "/auth/logout") {
    res.setHeader("Set-Cookie", "s=; Path=/; Max-Age=0");
    return json(res, 204);
  }
  if (p.startsWith("/api/")) {
    const u = userOf(req);
    if (!u) return json(res, 401, { code: "UNAUTHORIZED", message: "not logged in" });
    const scope = url.searchParams.get("scope") ?? "mine";
    let m;
    if (p === "/api/me") return json(res, 200, me(u));
    if (p === "/api/me/pubkeys") return json(res, 200, { pubkeys: [] });
    if (p === "/api/devpods" && url.searchParams.get("watch") === "true") return sse(req, res, () => undefined, 0);
    if (p === "/api/devpods") {
      const items = visible(u, scope);
      return json(res, 200, { items, pods: Object.fromEntries(items.map((d) => [d.metadata.name, podInfo[d.metadata.name]]).filter(([, v]) => v)) });
    }
    if (p === "/api/live/devpods") return sse(req, res, () => ({ ts: iso(), items: visible(u, scope).map((d) => live(d, devpods.indexOf(d))) }));
    if (p === "/api/admin/live/overview")
      return sse(req, res, () => ({
        ts: iso(),
        kpi: { cpuUsed: 300 + 50 * Math.sin(t()), cpuAllocatable: 1536, memUsed: 3000 * GiB, memAllocatable: 12288 * GiB, gpuUsed: 48, gpuTotal: 96, running: 330, pending: 12, failed: 17, stopped: 40, sessions: 57, sessionUsers: 31, rdma: 5e9 },
        nodes: nodes.map((n, i) => ({ name: n.name, ready: n.ready, cpuAllocatable: 128, cpuAllocated: n.cpu.allocated, cpuUsed: 40 + 30 * Math.abs(Math.sin(t() / 5 + i)), memAllocatable: 1024 * GiB, memAllocated: n.mem.allocated, memUsed: 400 * GiB, gpuTotal: 8, gpuUsed: n.gpu.used, devpods: n.devpods })),
        users: users.slice(0, 15).map((x, i) => ({ user: x.name, devpods: 10, running: 8, cpu: 20 - i, mem: (64 - i) * GiB, gpu: i % 4 })),
        attention: [
          { kind: "idle_gpu", uid: "uid-5", devpod: devpods[5].metadata.name, owner: devpods[5].spec.owner, since: iso(90000e3), detail: "holds 2 GPU, idle for 24h" },
          { kind: "pending_long", uid: "uid-34", devpod: devpods[34].metadata.name, owner: devpods[34].spec.owner, since: iso(900e3), detail: "pending for 15m" },
        ],
      }));
    if ((m = /^\/api\/devpods\/([^/]+)$/.exec(p))) {
      const d = devpods.find((x) => x.metadata.name === m[1]);
      if (!d) return json(res, 404, { code: "NOT_FOUND", message: "not found" });
      if (req.method === "PATCH" || req.method === "DELETE") return json(res, 200, { devpod: d, rebuild: false });
      return json(res, 200, { devpod: d, pod: podInfo[d.metadata.name] });
    }
    if ((m = /^\/api\/devpods\/([^/]+)\/stream$/.exec(p))) {
      const d = devpods.find((x) => x.metadata.name === m[1]);
      let first = true;
      const all = url.searchParams.get("procs") === "all";
      return sse(req, res, () => {
        const out = [{ kind: "metrics", metrics: samples(m[1]) }, { kind: "processes", processes: procs(all ? 40 : 10) }];
        if (first) {
          first = false;
          out.unshift({ kind: "devpod", type: "ADDED", detail: { devpod: d, pod: podInfo[m[1]] } });
          out.push({ kind: "event", type: "ADDED", event: { metadata: { uid: "e1" }, reason: "Started", message: "Started container dev", type: "Normal", count: 1, lastTimestamp: iso() } });
        }
        return out;
      });
    }
    if ((m = /^\/api\/devpods\/([^/]+)\/yaml$/.exec(p))) {
      const d = devpods.find((x) => x.metadata.name === m[1]);
      if (!d) return json(res, 404, { code: "NOT_FOUND", message: "not found" });
      if (req.method === "PUT") return json(res, 200, { devpod: d, rebuild: false });
      const yaml = `apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: ${d.metadata.name}\nspec:\n  owner: ${d.spec.owner}\n  running: true\n  pod:\n    spec:\n      containers:\n        - name: dev\n          image: ubuntu:24.04\n`;
      return json(res, 200, { yaml, running: true });
    }
    if ((m = /^\/api\/devpods\/([^/]+)\/pod$/.exec(p))) return json(res, 200, { pod: null });
    if ((m = /^\/api\/devpods\/([^/]+)\/metrics$/.exec(p))) {
      const n = 180;
      const ts = Array.from({ length: n }, (_, k) => Math.floor(t()) - (n - k) * 5);
      const wave = (a, b) => ts.map((x) => +(a + b * Math.abs(Math.sin(x / 300))).toFixed(2));
      return json(res, 200, { step: 5, ts, series: { cpu: wave(1, 6), cpu_limit: ts.map(() => 8), mem_anon: wave(8 * GiB, 8 * GiB), mem_file: wave(2 * GiB, 2 * GiB), mem_limit: ts.map(() => 32 * GiB), net_rx: wave(1e5, 1e6), net_tx: wave(1e4, 3e5), io_r: wave(0, 1e5), io_w: wave(0, 5e4) } });
    }
    if (/\/info$/.test(p)) return json(res, 200, { info: { ts: iso(), os: "Ubuntu 24.04.1 LTS", kernel: "6.8.0-45-generic", cpu_limit: 8, cpuset: "0-7", mem_limit: 32 * GiB, shell: "/bin/bash", agent_version: "v0.3.0" }, infoTs: iso() });
    if (/\/topology$/.test(p)) return json(res, 200, { node: null });
    // a config that comes a moment late (after the YAML editor has loaded),
    // as on a first visit: shared with alice (not hers), or hers
    if ((m = /^\/api\/configs\/(late-shared|late-mine)$/.exec(p))) {
      const mine = m[1] === "late-mine";
      const config = { name: m[1], owner: mine ? "alice" : "bob", title: m[1], description: "", sharedWith: [], public: false, official: false, variables: [], mine,
        updatedAt: iso(), content: "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nspec:\n  image: ubuntu:24.04 # from-the-config\n" };
      setTimeout(() => json(res, 200, config), 2500);
      return;
    }
    if (p === "/api/jobs" && req.method === "POST") {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        const r = JSON.parse(body);
        const waiting = jobs.filter((j) => j.phase === "Queued").length;
        if (r.dryRun) return json(res, 200, { position: waiting + 1, reason: "behind", expectedStart: new Date(Date.now() + 8 * 3600e3).toISOString(), cpus: 12 * (r.gpus || 1) });
        const j = { id: `new${String(jobs.length).padStart(2, "0")}`, title: r.title, owner: u, phase: "Queued", nodes: r.nodes, gpus: r.gpus, cpus: r.cpus ?? 12 * r.gpus,
          timeLimitSeconds: r.timeLimitSeconds ?? 86400, priority: r.priority ?? 0, preemptible: !!r.preemptible, submitted: iso(), position: waiting + 1, reason: "behind",
          detail: { head: "wait1" }, ranks: [], command: r.command, workdir: r.workdir, source: r.from };
        jobs.push(j);
        json(res, 201, jobView(j, u));
      });
      return;
    }
    if (p === "/api/jobs") {
      const mine = url.searchParams.get("mine") === "1";
      return json(res, 200, { items: jobs.filter((j) => !mine || j.owner === u).map((j) => jobView(j, u)) });
    }
    if (p === "/api/jobs/limits") return json(res, 200, { maxPriority: 100, unlimitedPriority: u === "admin", defaultTimeLimitSeconds: 86400, maxTimeLimitSeconds: 259200 });
    if ((m = /^\/api\/jobs\/([^/]+)$/.exec(p))) {
      const j = jobs.find((x) => x.id === m[1]);
      if (!j) return json(res, 404, { code: "NOT_FOUND", message: "no such job" });
      return json(res, 200, jobView(j, u));
    }
    // node reservations: the rules, six reservable machines, one of bob's
    if (p === "/api/reservations/rules")
      return json(res, 200, { granularitySeconds: 600, maxDurationSeconds: 0, maxAdvanceSeconds: 14 * 86400, clearNoticeSeconds: 1800, timezone: "Asia/Shanghai", now: iso() });
    if (p === "/api/nodes")
      return json(res, 200, {
        items: nodes.slice(0, 6).map((n) => ({ name: n.name, ready: n.ready, cpu: n.cpu, mem: n.mem, gpuModel: n.gpu.model, gpuTotal: n.gpu.total, gpuUsed: n.gpu.used,
          reservable: true, access: "ok", reservationRequired: false, pending: 0, mine: null, selectable: true })),
      });
    // a new one on g205 is refused: the machine is full then
    if (p === "/api/reservations" && req.method === "POST") {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => {
        const r = JSON.parse(body);
        if (r.node === "g205")
          return json(res, 409, { code: "NO_ROOM", message: "node g205 has at most 2 GPUs + 32 cores free to reserve in that time", detail: { free: { gpu: 2, cpu: 32 }, earliest: new Date(Date.now() + 6 * 3600e3).toISOString() } });
        json(res, 201, { id: "new", phase: "Upcoming", user: "alice", mine: true, canManage: true, ...r });
      });
      return;
    }
    // bob holds g200 whole; carol half of g201
    if (p === "/api/reservations")
      return json(res, 200, {
        items: [
          { id: "rb", node: "g200", user: "bob", start: new Date(Date.now() + 3600e3).toISOString(), end: new Date(Date.now() + 3 * 3600e3).toISOString(), phase: "Upcoming", mine: false, canManage: false, purpose: "跑基准" },
          { id: "rc", node: "g201", user: "carol", start: new Date(Date.now() + 3600e3).toISOString(), end: new Date(Date.now() + 3 * 3600e3).toISOString(), phase: "Upcoming", mine: false, canManage: false, gpu: 4, cpu: 64 },
        ],
      });
    if (p === "/api/admin/nodes") return json(res, 200, { items: nodes, live: true });
    // stored history for the CPU curves (before the node route: "recent" is no node)
    if (p === "/api/admin/nodes/recent") return json(res, 200, { step: 60, ts: [], items: {} });
    if ((m = /^\/api\/admin\/nodes\/([^/]+)$/.exec(p))) {
      const n = nodes.find((x) => x.name === m[1]);
      return json(res, 200, { node: n, devpods: devpods.slice(0, 6).map((d) => ({ name: d.metadata.name, owner: d.spec.owner, phase: d.status.phase, cpu: 2, mem: 4 * GiB, gpu: 0 })), live: true });
    }
    if (p === "/api/admin/users") return json(res, 200, { items: users, defaultQuota: { maxDevPods: 10, compute: { cpu: "16", memory: "64Gi" } } });
    if (p === "/api/admin/users:batch") {
      let body = "";
      req.on("data", (c) => (body += c));
      req.on("end", () => json(res, 200, { results: JSON.parse(body).names.map((name) => ({ name, ok: true })) }));
      return;
    }
    if (p.startsWith("/api/admin/users/")) return json(res, 204);
    if (p === "/api/audit/presence") return json(res, 200, { items: {} });
    if (p === "/api/admin/audit/policy-default") return json(res, 200, {});
    if (p.startsWith("/api/audit/")) return json(res, 200, { items: [], next: "" });
    if (p === "/api/search") return json(res, 200, { users: [], devpods: [], sessions: [], commands: [] });
    return json(res, 404, { code: "NOT_FOUND", message: `mock has no ${req.method} ${p}` });
  }
  // the SPA
  const file = path.join(dist, p);
  if (p !== "/" && file.startsWith(dist) && fs.existsSync(file) && fs.statSync(file).isFile()) {
    const ext = path.extname(file);
    const type = { ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".html": "text/html" }[ext] ?? "application/octet-stream";
    res.writeHead(200, { "Content-Type": type });
    return fs.createReadStream(file).pipe(res);
  }
  res.writeHead(200, { "Content-Type": "text/html" });
  fs.createReadStream(path.join(dist, "index.html")).pipe(res);
});
server.listen(PORT, () => console.log(`mock api on :${PORT}`));
