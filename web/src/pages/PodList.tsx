import { useEffect } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient, useMutation } from "@tanstack/react-query";
import { me, listDevPods, patchDevPod, watchDevPods, sshCommand, DevPod } from "../api";
import { Button, CopyRow, PhaseLabel, Shell, StatusCell } from "../ui";

function suffixOf(dp: DevPod) {
  const p = dp.spec.owner + "-";
  return dp.metadata.name.startsWith(p) ? dp.metadata.name.slice(p.length) : dp.metadata.name;
}

export default function PodList() {
  const qc = useQueryClient();
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const podsQ = useQuery({ queryKey: ["devpods"], queryFn: listDevPods });

  useEffect(
    () =>
      watchDevPods(
        (type, dp) => {
          qc.setQueryData(["devpods"], (old: { items: DevPod[] } | undefined) => {
            const items = old?.items ?? [];
            if (type === "DELETED") return { items: items.filter((d) => d.metadata.name !== dp.metadata.name) };
            const idx = items.findIndex((d) => d.metadata.name === dp.metadata.name);
            if (idx >= 0) {
              const next = [...items];
              next[idx] = dp;
              return { items: next };
            }
            return { items: [...items, dp] };
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
  const u = meQ.data?.usage;

  return (
    <Shell>
      <header className="mb-6 flex flex-wrap items-end justify-between gap-2">
        <div>
          <p className="eyebrow mb-1">Environments</p>
          <h1 className="mono text-2xl font-semibold tracking-tight">DevPods</h1>
        </div>
        {u && (
          <p className="mono text-xs text-muted">
            {u.devpods} total · <span className="text-run">{u.running} running</span>
            {u.compute.cpu && (
              <>
                {" "}· {u.compute.cpu}
                <span className="text-faint">/{meQ.data?.quota.compute?.cpu ?? "∞"}</span> cpu
              </>
            )}
          </p>
        )}
      </header>

      {items.length === 0 ? (
        <div className="rounded-2xl border border-dashed border-line-strong bg-surface px-6 py-16 text-center">
          <StatusCell phase="Stopped" className="mx-auto mb-3 !size-3" />
          <p className="text-sm text-muted">No environments yet.</p>
          <Link to="/devpods/new" className="mt-3 inline-flex rounded-lg bg-accent px-4 py-2 text-sm font-medium text-white hover:bg-accent-hi">
            Create your first
          </Link>
        </div>
      ) : (
        <ul className="overflow-hidden rounded-2xl border border-line bg-surface">
          {items.map((dp, i) => {
            const phase = dp.status?.phase;
            return (
              <li
                key={dp.metadata.name}
                className={cxRow(i)}
              >
                <div className="flex items-start gap-3">
                  <StatusCell phase={phase} className="mt-1.5" />
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                      <Link to={`/devpods/${dp.metadata.name}`} className="mono truncate text-sm font-medium text-ink hover:text-accent">
                        {dp.metadata.name}
                      </Link>
                      <PhaseLabel phase={phase} />
                    </div>
                    {dp.status?.phase === "Running" && meQ.data && (
                      <div className="mt-2">
                        <CopyRow value={sshCommand(meQ.data, dp.spec.owner, suffixOf(dp))} />
                      </div>
                    )}
                  </div>
                  <Button
                    size="sm"
                    onClick={() => toggle.mutate({ name: dp.metadata.name, running: !dp.spec.running })}
                    disabled={toggle.isPending}
                  >
                    {dp.spec.running ? "Hibernate" : "Wake"}
                  </Button>
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </Shell>
  );
}

function cxRow(i: number) {
  return "px-4 py-4 sm:px-5" + (i > 0 ? " border-t border-line" : "");
}
