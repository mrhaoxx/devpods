import { useQuery } from "@tanstack/react-query";
import { koreTopology } from "../api";
import { NodeZones, TopologyLegend } from "../kore";
import { BackLink, Card, Shell } from "../ui";

export default function AdminTopology() {
  const q = useQuery({ queryKey: ["kore-topology"], queryFn: koreTopology, refetchInterval: 4000 });
  const nodes = q.data?.nodes ?? [];

  return (
    <Shell>
      <BackLink />
      <header className="mb-1 mt-3 flex flex-wrap items-center justify-between gap-3">
        <h1 className="mono text-xl font-semibold tracking-tight">CPU topology</h1>
        <TopologyLegend />
      </header>
      <p className="mb-5 text-xs text-faint">
        Each column is one physical core; stacked cells are its hyperthreads (SMT siblings).
      </p>

      {q.isError && <Card className="p-5"><p className="text-sm text-muted">Kore topology is unavailable.</p></Card>}
      {nodes.length === 0 && !q.isError && <Card className="p-5"><p className="text-sm text-faint">No node topology reported yet.</p></Card>}

      <div className="space-y-4">
        {nodes.map((node) => (
          <Card key={node.node} className="p-5">
            <div className="mb-4 flex flex-wrap items-baseline justify-between gap-2">
              <h2 className="mono text-sm font-semibold text-ink">{node.node}</h2>
              {node.reservedCpus && <span className="mono text-xs text-faint">reserved {node.reservedCpus}</span>}
            </div>
            <NodeZones node={node} reveal />
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
        ))}
      </div>
    </Shell>
  );
}
