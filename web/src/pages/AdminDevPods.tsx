import { Link } from "react-router-dom";
import { useQuery, useQueryClient, useMutation } from "@tanstack/react-query";
import { listAllDevPods, patchDevPod, AdminDevPod } from "../api";
import { BackLink, Button, PhaseLabel, Shell, StatusCell } from "../ui";

export default function AdminDevPods() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["admin-devpods"], queryFn: listAllDevPods, refetchInterval: 8000 });
  const toggle = useMutation({
    mutationFn: ({ name, running }: { name: string; running: boolean }) => patchDevPod(name, running),
    onSettled: () => qc.invalidateQueries({ queryKey: ["admin-devpods"] }),
  });

  const items = q.data?.items ?? [];
  const running = items.filter((d) => d.running).length;

  return (
    <Shell>
      <BackLink />
      <header className="mb-5 mt-3 flex flex-wrap items-end justify-between gap-2">
        <h1 className="mono text-xl font-semibold tracking-tight">All DevPods</h1>
        <p className="mono text-xs text-muted">
          {items.length} total · <span className="text-run">{running} running</span>
        </p>
      </header>

      <ul className="overflow-hidden rounded-2xl border border-line bg-surface">
        {items.map((dp: AdminDevPod, i) => (
          <li key={dp.name} className={"flex items-center gap-3 px-4 py-3" + (i > 0 ? " border-t border-line" : "")}>
            <StatusCell phase={dp.phase} />
            <div className="min-w-0 flex-1">
              <Link to={`/devpods/${dp.name}`} className="mono truncate text-sm text-ink hover:text-accent">
                {dp.name}
              </Link>
              <div className="mono text-xs text-faint">
                {dp.owner}
                {(dp.cpu || dp.memory) && <span className="text-muted"> · {[dp.cpu && `${dp.cpu} cpu`, dp.memory].filter(Boolean).join(" / ")}</span>}
                {dp.storage && <span className="text-muted"> · {dp.storage}</span>}
              </div>
            </div>
            <div className="hidden sm:block"><PhaseLabel phase={dp.phase} /></div>
            <Button
              size="sm"
              onClick={() => toggle.mutate({ name: dp.name, running: !dp.running })}
              disabled={toggle.isPending}
            >
              {dp.running ? "Hibernate" : "Wake"}
            </Button>
          </li>
        ))}
        {items.length === 0 && <li className="p-8 text-center text-sm text-faint">No DevPods.</li>}
      </ul>
    </Shell>
  );
}
