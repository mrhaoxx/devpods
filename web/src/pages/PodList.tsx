import { useEffect } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient, useMutation } from "@tanstack/react-query";
import { auditPresence, me, listDevPods, patchDevPod, watchDevPods, DevPod, PodInfo } from "../api";
import { DevPodTable } from "../components/DevPodTable";
import { Shell } from "../ui";

type ListData = { items: DevPod[]; pods: Record<string, PodInfo> };

export default function PodList() {
  const qc = useQueryClient();
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const podsQ = useQuery({ queryKey: ["devpods"], queryFn: listDevPods });
  const presenceQ = useQuery({
    queryKey: ["presence"],
    queryFn: () => auditPresence(false),
    enabled: !!meQ.data?.features.audit,
    refetchInterval: 10_000,
  });

  useEffect(
    () =>
      watchDevPods(
        (type, dp, pod) => {
          qc.setQueryData(["devpods"], (old: ListData | undefined) => {
            const items = old?.items ?? [];
            const pods = { ...(old?.pods ?? {}) };
            const name = dp.metadata.name;
            if (type === "DELETED") {
              delete pods[name];
              return { items: items.filter((d) => d.metadata.name !== name), pods };
            }
            if (pod) pods[name] = pod;
            else delete pods[name];
            const idx = items.findIndex((d) => d.metadata.name === name);
            const next = [...items];
            if (idx >= 0) next[idx] = dp;
            else next.push(dp);
            return { items: next, pods };
          });
        },
        () => qc.invalidateQueries({ queryKey: ["devpods"] }),
      ),
    [qc],
  );

  const toggle = useMutation({
    mutationFn: ({ name, running }: { name: string; running: boolean }) => patchDevPod(name, running),
    onSettled: () => qc.invalidateQueries({ queryKey: ["devpods"] }),
  });

  const items = podsQ.data?.items ?? [];

  return (
    <Shell wide>
      <header className="mb-4 flex flex-wrap items-end justify-between gap-2">
        <div>
          <p className="eyebrow mb-1">Environments</p>
          <h1 className="mono text-2xl font-semibold tracking-tight">DevPods</h1>
        </div>
      </header>

      {meQ.data && podsQ.data && items.length === 0 ? (
        <div className="rounded-2xl border border-dashed border-line-strong bg-surface px-6 py-16 text-center">
          <p className="text-sm text-muted">No environments yet.</p>
          <Link to="/devpods/new" className="mt-3 inline-flex rounded-lg bg-accent px-4 py-2 text-sm font-medium text-white hover:bg-accent-hi">
            Create your first
          </Link>
        </div>
      ) : (
        meQ.data && (
          <DevPodTable
            items={items}
            pods={podsQ.data?.pods ?? {}}
            me={meQ.data}
            presence={presenceQ.data?.items}
            onToggle={(name, running) => toggle.mutate({ name, running })}
            toggling={toggle.isPending}
          />
        )
      )}
    </Shell>
  );
}
