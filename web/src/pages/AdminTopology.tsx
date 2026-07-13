import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { koreTopology, parseCpuList, KoreNode, KoreZone } from "../api";
import { BackLink, Card, Shell, cx } from "../ui";

type Kind = "exclusive" | "pool" | "reserved" | "shared" | "free";
type CoreInfo = { kind: Kind; label: string; pod?: string };

const kindCell: Record<Kind, string> = {
  exclusive: "bg-accent",
  pool: "bg-warm",
  reserved: "bg-idle",
  shared: "bg-idle/40",
  free: "border border-line-strong bg-transparent",
};

// Resolve every core in a node to its occupant, with a fixed priority.
function coreMap(node: KoreNode): Map<number, CoreInfo> {
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

function Cell({ core, info }: { core: number; info: CoreInfo }) {
  const title = `cpu ${core} · ${info.label}`;
  const box = <span title={title} className={cx("block size-3.5 rounded-[3px]", kindCell[info.kind])} />;
  if (info.kind === "exclusive" && info.pod) {
    return <Link to={`/devpods/${info.pod}`} title={title}>{box}</Link>;
  }
  return box;
}

function Zone({ zone, cores }: { zone: KoreZone; cores: Map<number, CoreInfo> }) {
  const free = parseCpuList(zone.freeCpus);
  const all = [...parseCpuList(zone.cpus)].sort((a, b) => a - b);
  const info = (c: number): CoreInfo =>
    cores.get(c) ?? (free.has(c) ? { kind: "free", label: "free" } : { kind: "shared", label: "shared" });

  // Each column is a physical core; stacked cells are its SMT siblings
  // (hyperthreads). No SMT info → one cell per column.
  const groups = zone.smtSiblings?.length
    ? zone.smtSiblings.map((g) => [...g].sort((a, b) => a - b)).sort((a, b) => a[0] - b[0])
    : all.map((c) => [c]);
  const smt = zone.smtSiblings?.length ? Math.max(...zone.smtSiblings.map((g) => g.length)) : 1;

  return (
    <div>
      <div className="mb-2 flex items-baseline gap-2">
        <span className="eyebrow">numa {zone.id}</span>
        <span className="mono text-xs text-faint">
          {all.length} cores{zone.memory ? ` · ${zone.memory}` : ""} · {free.size} free
          {smt > 1 ? ` · ${smt}-way SMT` : ""}
        </span>
      </div>
      <div className="flex flex-wrap gap-[3px]">
        {groups.map((g, i) => (
          <div key={i} className="flex flex-col gap-[3px]">
            {g.map((c) => (
              <Cell key={c} core={c} info={info(c)} />
            ))}
          </div>
        ))}
      </div>
    </div>
  );
}

function Legend() {
  const items: [Kind, string][] = [
    ["exclusive", "pinned"],
    ["pool", "pool"],
    ["shared", "shared"],
    ["reserved", "reserved"],
    ["free", "free"],
  ];
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1.5 text-xs text-muted">
      {items.map(([k, label]) => (
        <span key={k} className="inline-flex items-center gap-1.5">
          <span className={cx("block size-3 rounded-[3px]", kindCell[k])} /> {label}
        </span>
      ))}
    </div>
  );
}

export default function AdminTopology() {
  const q = useQuery({ queryKey: ["kore-topology"], queryFn: koreTopology, refetchInterval: 4000 });
  const nodes = q.data?.nodes ?? [];

  return (
    <Shell>
      <BackLink />
      <header className="mb-1 mt-3 flex flex-wrap items-center justify-between gap-3">
        <h1 className="mono text-xl font-semibold tracking-tight">CPU topology</h1>
        <Legend />
      </header>
      <p className="mb-5 text-xs text-faint">
        Each column is one physical core; stacked cells are its hyperthreads (SMT siblings).
      </p>

      {q.isError && <Card className="p-5"><p className="text-sm text-muted">Kore topology is unavailable.</p></Card>}
      {nodes.length === 0 && !q.isError && <Card className="p-5"><p className="text-sm text-faint">No node topology reported yet.</p></Card>}

      <div className="space-y-4">
        {nodes.map((node) => {
          const cores = coreMap(node);
          return (
            <Card key={node.node} className="p-5">
              <div className="mb-4 flex flex-wrap items-baseline justify-between gap-2">
                <h2 className="mono text-sm font-semibold text-ink">{node.node}</h2>
                {node.reservedCpus && <span className="mono text-xs text-faint">reserved {node.reservedCpus}</span>}
              </div>
              <div className="space-y-4">
                {node.zones.map((z) => (
                  <Zone key={z.id} zone={z} cores={cores} />
                ))}
              </div>
              {node.pools.length > 0 && (
                <div className="mt-4 border-t border-line pt-3">
                  <p className="eyebrow mb-2">pools</p>
                  <ul className="space-y-2.5 text-xs">
                    {node.pools.map((p) => (
                      <li key={p.name}>
                        <div className="mono text-muted">
                          <span className="text-warm">{p.name}</span> · {p.cpuset}
                          {p.numa?.length ? ` · numa ${p.numa.join(",")}` : ""} · {p.members?.length ?? 0} members
                        </div>
                        {p.members && p.members.length > 0 && (
                          <div className="mt-1 flex flex-wrap gap-1">
                            {p.members.map((m) => {
                              const name = m.includes("/") ? m.split("/").pop()! : m;
                              return (
                                <span key={m} className="mono rounded bg-warm-soft px-1.5 py-0.5 text-[11px] text-warm" title={m}>
                                  {name}
                                </span>
                              );
                            })}
                          </div>
                        )}
                      </li>
                    ))}
                  </ul>
                </div>
              )}
            </Card>
          );
        })}
      </div>
    </Shell>
  );
}
