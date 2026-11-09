import { useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { DevPod, Me, PodInfo, Presence, sshCommand } from "../api";
import { ALL_COLUMNS, ColumnKey, visibleColumns } from "../columns";
import { failureRetryIn, fmtAge, fmtBytes, fmtDuration, sumLimits } from "../format";
import { Button, copyText, cx, phaseTone, StatusCell } from "../ui";

// useNow re-renders every intervalMs (the Up column ticks).
export function useNow(intervalMs = 1000): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}

// useWidth tracks an element's width for responsive columns.
function useWidth<T extends HTMLElement>(): [React.RefObject<T>, number] {
  const ref = useRef<T>(null);
  const [w, setW] = useState(1200);
  useEffect(() => {
    if (!ref.current) return;
    const ro = new ResizeObserver((es) => setW(es[0].contentRect.width));
    ro.observe(ref.current);
    return () => ro.disconnect();
  }, []);
  return [ref, w];
}

const toneText: Record<string, string> = { run: "text-run", warm: "text-warm", idle: "text-idle", fail: "text-fail" };

function suffixOf(dp: DevPod) {
  const p = dp.spec.owner + "-";
  return dp.metadata.name.startsWith(p) ? dp.metadata.name.slice(p.length) : dp.metadata.name;
}

function imageOf(dp: DevPod): string {
  const img = dp.spec.pod?.spec?.containers?.[0]?.image ?? "";
  return img.split("/").pop() ?? img;
}

// statusLine is the merged cell for rows that are not running.
export function statusLine(dp: DevPod, now: Date): string {
  const st = dp.status ?? {};
  switch (st.phase) {
    case "Stopped": {
      const since = st.hibernatedAt ? `hibernated ${fmtAge(st.hibernatedAt, now)} ago` : "hibernated";
      return dp.spec.persistence ? `${since} · home PVC ${dp.spec.persistence.size} kept` : since;
    }
    case "Failed": {
      const retry =
        st.retryCount && st.lastFailureAt ? ` · retry #${st.retryCount} in ${failureRetryIn(st.retryCount, st.lastFailureAt, now)}s` : "";
      return `${st.message || "failed"}${retry}`;
    }
    default:
      return st.message || "starting…";
  }
}

const HEAD: Record<ColumnKey, string> = {
  name: "Name",
  phase: "Phase",
  cpu: "CPU",
  memory: "Memory",
  gpu: "GPU",
  net: "Net ↓/↑",
  rdma: "RDMA ↓/↑",
  sess: "Sess",
  up: "Up",
  image: "Image",
  ssh: "",
  actions: "",
};

// Columns between phase and ssh collapse into one status cell for rows
// that are not running.
const METRIC_COLS: ColumnKey[] = ["cpu", "memory", "gpu", "net", "rdma", "sess", "up", "image"];

export function DevPodTable(props: {
  items: DevPod[];
  pods: Record<string, PodInfo>;
  me: Me;
  presence?: Record<string, Presence>;
  showOwner?: boolean;
  onToggle: (name: string, running: boolean) => void;
  toggling?: boolean;
}) {
  const { items, pods, me, presence, showOwner, onToggle, toggling } = props;
  const now = useNow();
  const [ref, width] = useWidth<HTMLDivElement>();
  const [filter, setFilter] = useState("");
  const [phase, setPhase] = useState("All");

  // Phase 1: no live metrics yet (net / rdma arrive with telemetry).
  const available = ALL_COLUMNS.filter((c) => c !== "net" && c !== "rdma" && (c !== "sess" || !!me.features.audit));
  const cols = visibleColumns(width, available);
  const metricSpan = cols.filter((c) => METRIC_COLS.includes(c)).length;

  const counts = useMemo(() => {
    const c: Record<string, number> = { All: items.length };
    for (const dp of items) {
      const p = dp.status?.phase ?? "Pending";
      c[p] = (c[p] ?? 0) + 1;
    }
    return c;
  }, [items]);

  const shown = items
    .filter((dp) => phase === "All" || (dp.status?.phase ?? "Pending") === phase)
    .filter((dp) => {
      if (!filter) return true;
      const f = filter.toLowerCase();
      return dp.metadata.name.toLowerCase().includes(f) || imageOf(dp).toLowerCase().includes(f) || dp.spec.owner.includes(f);
    })
    .sort((a, b) => (showOwner && a.spec.owner !== b.spec.owner ? a.spec.owner.localeCompare(b.spec.owner) : a.metadata.name.localeCompare(b.metadata.name)));

  const totals = useMemo(() => {
    let cpu = 0;
    let mem = 0;
    let gpu = 0;
    for (const dp of items) {
      if (!dp.spec.running) continue;
      const l = sumLimits(dp.spec.pod?.spec);
      cpu += l.cpu;
      mem += l.memory;
      gpu += l.gpu;
    }
    return { cpu: Math.round(cpu * 10) / 10, mem, gpu };
  }, [items]);

  const cell = "whitespace-nowrap px-2 py-1";

  const render = (c: ColumnKey, dp: DevPod) => {
    const name = dp.metadata.name;
    const lim = sumLimits(dp.spec.pod?.spec);
    const pod = pods[name];
    switch (c) {
      case "name":
        return (
          <td key={c} className={cell}>
            <span className="inline-flex items-center gap-1.5">
              <StatusCell phase={dp.status?.phase} />
              <Link to={`/devpods/${name}`} className="font-semibold text-ink hover:text-accent">
                {showOwner ? name : suffixOf(dp)}
              </Link>
              {showOwner && <span className="text-faint">{dp.spec.owner}</span>}
            </span>
          </td>
        );
      case "phase":
        return (
          <td key={c} className={cx(cell, toneText[phaseTone(dp.status?.phase)])}>
            {dp.status?.phase ?? "Pending"}
          </td>
        );
      case "cpu":
        return (
          <td key={c} className={cell}>
            {lim.cpu ? lim.cpu : <span className="text-faint">—</span>}
            {lim.cpu > 0 && <span className="text-faint"> cores</span>}
          </td>
        );
      case "memory":
        return <td key={c} className={cell}>{lim.memory ? fmtBytes(lim.memory) : <span className="text-faint">—</span>}</td>;
      case "gpu":
        return (
          <td key={c} className={cell}>
            {lim.gpu ? (
              <span className="inline-flex items-center gap-0.5">
                {Array.from({ length: Math.min(lim.gpu, 8) }).map((_, i) => (
                  <i key={i} className="inline-block h-[11px] w-[7px] rounded-[2px] bg-run/70" />
                ))}
                <span className="ml-1">{lim.gpu}</span>
              </span>
            ) : (
              <span className="text-faint">—</span>
            )}
          </td>
        );
      case "sess": {
        const p = presence?.[name];
        return (
          <td key={c} className={cell}>
            {p ? p.count : 0}
            {p && p.users.length > 0 && <span className="text-faint"> {p.users.join(",")}</span>}
          </td>
        );
      }
      case "up":
        return (
          <td key={c} className={cx(cell, "text-muted")}>
            {pod?.startedAt ? fmtDuration(now.getTime() - new Date(pod.startedAt).getTime()) : "—"}
          </td>
        );
      case "image":
        return <td key={c} className={cx(cell, "max-w-[14rem] truncate text-muted")}>{imageOf(dp)}</td>;
      case "ssh":
        return (
          <td key={c} className={cell}>
            {dp.status?.phase === "Running" && (
              <button
                className="rounded border border-line px-1.5 text-muted hover:border-line-strong hover:text-ink"
                title={sshCommand(me, dp.spec.owner, suffixOf(dp))}
                onClick={() => copyText(sshCommand(me, dp.spec.owner, suffixOf(dp)))}
              >
                ⧉ ssh
              </button>
            )}
          </td>
        );
      case "actions":
        return (
          <td key={c} className={cx(cell, "text-right")}>
            <Button size="sm" onClick={() => onToggle(name, !dp.spec.running)} disabled={toggling}>
              {dp.spec.running ? "Hibernate" : "Wake"}
            </Button>
          </td>
        );
      default:
        return <td key={c} className={cell} />;
    }
  };

  return (
    <div ref={ref} className="mono text-xs">
      <div className="mb-2.5 flex flex-wrap items-center gap-2">
        <input
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="filter name / image…"
          className="w-44 rounded-md border border-line-strong bg-surface px-2 py-1 text-ink placeholder:text-faint"
        />
        {["All", "Running", "Pending", "Stopped", "Failed"].map((p) =>
          p === "All" || counts[p] ? (
            <button
              key={p}
              onClick={() => setPhase(p)}
              className={cx(
                "rounded-full border px-2 py-0.5",
                phase === p ? "border-accent/40 bg-accent-soft text-accent" : "border-line bg-surface text-muted hover:text-ink",
              )}
            >
              {p} {counts[p] ?? 0}
            </button>
          ) : null,
        )}
        <span className="ml-auto text-muted">
          cpu <b className="font-semibold text-ink">{totals.cpu}</b>
          {!showOwner && me.quota.compute?.cpu && <span className="text-faint">/{me.quota.compute.cpu}</span>} · mem{" "}
          <b className="font-semibold text-ink">{fmtBytes(totals.mem)}</b>
          {!showOwner && me.quota.compute?.memory && <span className="text-faint">/{me.quota.compute.memory}</span>}
          {totals.gpu > 0 && (
            <>
              {" "}· gpu <b className="font-semibold text-ink">{totals.gpu}</b>
            </>
          )}
        </span>
      </div>

      <div className="overflow-hidden rounded-lg border border-line bg-surface">
        <table className="w-full border-collapse">
          <thead>
            <tr className="border-b border-line bg-sunk text-left text-[10.5px] uppercase tracking-wider text-faint">
              {cols.map((c) => (
                <th key={c} className="whitespace-nowrap px-2 py-1.5 font-medium">
                  {c === "name" && showOwner ? "Name / Owner" : HEAD[c]}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {shown.map((dp) => {
              const running = dp.status?.phase === "Running";
              return (
                <tr key={dp.metadata.name} className="h-[30px] border-b border-line/60 last:border-0 hover:bg-sunk/50">
                  {cols.map((c, i) => {
                    if (running || !METRIC_COLS.includes(c)) return render(c, dp);
                    // first metric column of a non-running row: one merged status cell
                    const first = cols.findIndex((k) => METRIC_COLS.includes(k));
                    if (i !== first) return null;
                    return (
                      <td key="status" colSpan={metricSpan} className={cx(cell, dp.status?.phase === "Failed" ? "text-fail" : "text-muted")}>
                        {statusLine(dp, now)}
                      </td>
                    );
                  })}
                </tr>
              );
            })}
            {shown.length === 0 && (
              <tr>
                <td colSpan={cols.length} className="px-4 py-10 text-center text-faint">
                  {items.length === 0 ? "No environments yet." : "Nothing matches the filter."}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
