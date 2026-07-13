import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { patchDevPod, deleteDevPod, me, sshCommand, sshConfig, watchDevPod, devpodTopology, parseCpuList, DevPodDetail, K8sEvent } from "../api";
import { BackLink, Button, Card, CopyBlock, CopyRow, PhaseLabel, Shell, cx } from "../ui";
import { NodeZones, TopologyLegend } from "../kore";

function fmtTime(ts?: string): string {
  if (!ts) return "";
  return new Date(ts).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

export default function PodDetail() {
  const { name = "" } = useParams();
  const nav = useNavigate();
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const topoQ = useQuery({ queryKey: ["devpod-topo", name], queryFn: () => devpodTopology(name), refetchInterval: 6000 });

  const [detail, setDetail] = useState<DevPodDetail | null>(null);
  const [eventsByUID, setEventsByUID] = useState<Record<string, K8sEvent>>({});
  const pendingRef = useRef<{ type: string; ev: K8sEvent }[]>([]);
  const rafRef = useRef(0);
  const flushEvents = useCallback(() => {
    rafRef.current = 0;
    const batch = pendingRef.current.splice(0);
    if (!batch.length) return;
    setEventsByUID((m) => {
      const next = { ...m };
      for (const { type, ev } of batch) {
        if (type === "DELETED") delete next[ev.metadata.uid];
        else next[ev.metadata.uid] = ev;
      }
      return next;
    });
  }, []);

  useEffect(() => {
    setDetail(null);
    setEventsByUID({});
    pendingRef.current = [];
    return watchDevPod(name, {
      onDetail: (d) => setDetail(d),
      onEvent: (type, ev) => {
        pendingRef.current.push({ type, ev });
        if (!rafRef.current) rafRef.current = requestAnimationFrame(flushEvents);
      },
    });
  }, [name, flushEvents]);

  const events = useMemo(() => {
    const byKey = new Map<string, K8sEvent & { totalCount: number }>();
    for (const ev of Object.values(eventsByUID)) {
      const key = `${ev.reason}:${ev.message}`;
      const ts = ev.lastTimestamp ?? "";
      const existing = byKey.get(key);
      if (!existing || ts > (existing.lastTimestamp ?? "")) {
        byKey.set(key, { ...ev, totalCount: (existing?.totalCount ?? 0) + (ev.count ?? 1) });
      } else {
        existing.totalCount += ev.count ?? 1;
      }
    }
    return [...byKey.values()].sort((a, b) => (b.lastTimestamp ?? "").localeCompare(a.lastTimestamp ?? ""));
  }, [eventsByUID]);

  if (!detail) return <Shell><p className="mono py-16 text-center text-sm text-faint">loading…</p></Shell>;
  const dp = detail.devpod;
  const binding = detail.binding;
  const owner = dp.spec.owner;
  const suffix = dp.metadata.name.startsWith(owner + "-") ? dp.metadata.name.slice(owner.length + 1) : dp.metadata.name;

  return (
    <Shell>
      <BackLink />
      <header className="mb-6 mt-3 flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-3">
          <h1 className="mono text-xl font-semibold tracking-tight">{dp.metadata.name}</h1>
          <PhaseLabel phase={dp.status?.phase} />
        </div>
        <div className="flex gap-2">
          <Button size="sm" onClick={() => patchDevPod(name, !dp.spec.running)}>
            {dp.spec.running ? "Hibernate" : "Wake"}
          </Button>
          <Button
            size="sm"
            variant="danger"
            onClick={() => {
              if (confirm(`Delete ${dp.metadata.name}? The home volume is lost.`)) deleteDevPod(name).then(() => nav("/"));
            }}
          >
            Delete
          </Button>
        </div>
      </header>

      <Card className="mb-4 p-5">
        <p className="eyebrow mb-3">Access</p>
        <dl className="grid gap-x-6 gap-y-3 sm:grid-cols-[7rem_1fr]">
          <dt className="text-sm text-muted">SSH</dt>
          <dd>{meQ.data ? <CopyRow value={sshCommand(meQ.data, owner, suffix)} /> : <span className="text-faint">—</span>}</dd>
          {dp.status?.message && (
            <>
              <dt className="text-sm text-muted">Message</dt>
              <dd className="text-sm text-fail">{dp.status.message}</dd>
            </>
          )}
          {meQ.data && (
            <>
              <dt className="text-sm text-muted">
                SSH config
                <span className="mt-0.5 block text-xs text-faint">→ ssh {dp.metadata.name}</span>
              </dt>
              <dd><CopyBlock value={sshConfig(meQ.data, owner, suffix)} /></dd>
            </>
          )}
        </dl>
      </Card>

      {topoQ.data?.node && (
        <Card className="mb-4 p-5">
          <div className="mb-1 flex flex-wrap items-center justify-between gap-2">
            <div className="flex items-baseline gap-2">
              <p className="eyebrow">Node layout</p>
              <span className="mono text-xs text-faint">{topoQ.data.node.node}</span>
            </div>
            <TopologyLegend mine />
          </div>
          <p className="mb-3 text-xs text-muted">
            {binding?.pool
              ? `Shared pool ${binding.pool} · ${binding.poolSize} cores`
              : binding?.allocatedCpuset
                ? `Pinned · cores ${binding.allocatedCpuset}${binding.reservedNuma ? ` · NUMA ${binding.reservedNuma}` : ""}`
                : "Your cores are highlighted."}
          </p>
          <NodeZones node={topoQ.data.node} reveal={false} mine={parseCpuList(topoQ.data.cpuset)} />
        </Card>
      )}
      {topoQ.data?.node === null && binding?.allocatedCpuset === undefined && binding?.pool === undefined && binding && (
        <Card className="mb-4 p-5">
          <p className="eyebrow mb-1">Compute · Kore</p>
          <p className="text-sm text-warm">binding pending</p>
        </Card>
      )}

      <Card className="p-5">
        <div className="mb-3 flex items-center gap-2">
          <p className="eyebrow">Events</p>
          <span className="inline-flex items-center gap-1 text-[10px] text-run">
            <span className="size-1.5 rounded-full bg-run pulse" /> live
          </span>
        </div>
        <ul className="space-y-1.5">
          {events.map((e, i) => (
            <li key={i} className="flex gap-2 text-xs">
              <time className="mono shrink-0 text-faint">{fmtTime(e.lastTimestamp)}</time>
              <span className={cx("shrink-0 font-medium", e.type === "Warning" ? "text-fail" : "text-muted")}>
                {e.reason}
                {e.totalCount > 1 ? ` ×${e.totalCount}` : ""}
              </span>
              <span className="min-w-0 text-ink/80">{e.message}</span>
            </li>
          ))}
          {events.length === 0 && <li className="text-xs text-faint">no events yet</li>}
        </ul>
      </Card>
    </Shell>
  );
}
