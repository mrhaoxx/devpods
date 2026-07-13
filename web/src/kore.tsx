import { Link } from "react-router-dom";
import { KoreNode, KoreZone, parseCpuList } from "./api";
import { cx } from "./ui";

type Kind = "exclusive" | "pool" | "reserved" | "shared" | "free";
type CoreInfo = { kind: Kind; label: string; pod?: string };

// Resolve every occupied core (reserved < pool < exclusive priority).
export function coreMap(node: KoreNode): Map<number, CoreInfo> {
  const m = new Map<number, CoreInfo>();
  for (const c of parseCpuList(node.reservedCpus)) m.set(c, { kind: "reserved", label: "system reserved" });
  for (const p of node.pools) {
    for (const c of parseCpuList(p.cpuset)) m.set(c, { kind: "pool", label: `pool ${p.name}` });
  }
  for (const a of node.allocations) {
    const name = a.devpod ?? (a.pod.includes("/") ? a.pod.split("/").pop()! : a.pod);
    for (const c of parseCpuList(a.cpuset)) m.set(c, { kind: "exclusive", label: `${name} / ${a.container}`, pod: a.devpod });
  }
  return m;
}

// reveal=false anonymizes other tenants (owner view); mine = the
// caller's own cores, always highlighted.
function Cell({ core, info, mine, reveal }: { core: number; info: CoreInfo; mine: Set<number>; reveal: boolean }) {
  const isMine = mine.has(core);
  let cls: string;
  let title: string;
  let pod: string | undefined;
  if (isMine) {
    cls = "bg-accent ring-2 ring-accent/30";
    title = `cpu ${core} · your core`;
  } else if (info.kind === "exclusive") {
    cls = reveal ? "bg-accent" : "bg-idle";
    title = `cpu ${core} · ${reveal ? info.label : "allocated"}`;
    pod = reveal ? info.pod : undefined;
  } else if (info.kind === "pool") {
    cls = "bg-warm";
    title = `cpu ${core} · ${reveal ? info.label : "pool"}`;
  } else if (info.kind === "reserved") {
    cls = "bg-idle";
    title = `cpu ${core} · system reserved`;
  } else if (info.kind === "shared") {
    cls = "bg-idle/40";
    title = `cpu ${core} · shared`;
  } else {
    cls = "border border-line-strong bg-transparent";
    title = `cpu ${core} · free`;
  }
  const box = <span title={title} className={cx("block size-3.5 rounded-[3px]", cls)} />;
  if (pod) return <Link to={`/devpods/${pod}`} title={title}>{box}</Link>;
  return box;
}

function Zone({ zone, cores, mine, reveal }: { zone: KoreZone; cores: Map<number, CoreInfo>; mine: Set<number>; reveal: boolean }) {
  const free = parseCpuList(zone.freeCpus);
  const all = [...parseCpuList(zone.cpus)].sort((a, b) => a - b);
  const info = (c: number): CoreInfo =>
    cores.get(c) ?? (free.has(c) ? { kind: "free", label: "free" } : { kind: "shared", label: "shared" });
  const groups = zone.smtSiblings?.length
    ? zone.smtSiblings.map((g) => [...g].sort((a, b) => a - b)).sort((a, b) => a[0] - b[0])
    : all.map((c) => [c]);
  const smt = zone.smtSiblings?.length ? Math.max(...zone.smtSiblings.map((g) => g.length)) : 1;

  return (
    <div>
      <div className="mb-2 flex items-baseline gap-2">
        <span className="eyebrow">numa {zone.id}</span>
        <span className="mono text-xs text-faint">
          {all.length} cores{zone.memory ? ` · ${zone.memory}` : ""} · {free.size} free{smt > 1 ? ` · ${smt}-way SMT` : ""}
        </span>
      </div>
      <div className="flex flex-wrap gap-[3px]">
        {groups.map((g, i) => (
          <div key={i} className="flex flex-col gap-[3px]">
            {g.map((c) => (
              <Cell key={c} core={c} info={info(c)} mine={mine} reveal={reveal} />
            ))}
          </div>
        ))}
      </div>
    </div>
  );
}

export function NodeZones({ node, mine, reveal }: { node: KoreNode; mine?: Set<number>; reveal: boolean }) {
  const cores = coreMap(node);
  const mineSet = mine ?? new Set<number>();
  return (
    <div className="space-y-4">
      {node.zones.map((z) => (
        <Zone key={z.id} zone={z} cores={cores} mine={mineSet} reveal={reveal} />
      ))}
    </div>
  );
}

export function TopologyLegend({ mine }: { mine?: boolean }) {
  const items: [string, string][] = mine
    ? [["bg-accent ring-2 ring-accent/30", "your cores"], ["bg-warm", "shared pool"], ["bg-idle", "allocated"], ["bg-idle/40", "shared"], ["border border-line-strong bg-transparent", "free"]]
    : [["bg-accent", "pinned"], ["bg-warm", "shared pool"], ["bg-idle/40", "shared"], ["bg-idle", "reserved"], ["border border-line-strong bg-transparent", "free"]];
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1.5 text-xs text-muted">
      {items.map(([c, label]) => (
        <span key={label} className="inline-flex items-center gap-1.5">
          <span className={cx("block size-3 rounded-[3px]", c)} /> {label}
        </span>
      ))}
    </div>
  );
}
