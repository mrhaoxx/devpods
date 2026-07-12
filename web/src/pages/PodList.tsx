import { useEffect } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient, useMutation } from "@tanstack/react-query";
import { me, listDevPods, patchDevPod, watchDevPods, DevPod } from "../api";

const phaseColor: Record<string, string> = {
  Running: "bg-green-100 text-green-800",
  Pending: "bg-yellow-100 text-yellow-800",
  Stopped: "bg-slate-100 text-slate-600",
  Failed: "bg-red-100 text-red-800",
};

export default function PodList() {
  const qc = useQueryClient();
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const podsQ = useQuery({ queryKey: ["devpods"], queryFn: listDevPods });

  useEffect(
    () =>
      watchDevPods(
        () => qc.invalidateQueries({ queryKey: ["devpods"] }),
        () => qc.invalidateQueries({ queryKey: ["devpods"] }),
      ),
    [qc],
  );

  const toggle = useMutation({
    mutationFn: ({ name, running }: { name: string; running: boolean }) => patchDevPod(name, running),
    onSettled: () => qc.invalidateQueries({ queryKey: ["devpods"] }),
  });

  return (
    <main className="mx-auto max-w-4xl p-8">
      <header className="mb-6 flex items-center justify-between">
        <h1 className="text-xl font-semibold">My DevPods</h1>
        <nav className="flex items-center gap-3 text-sm">
          <Link className="text-blue-600 hover:underline" to="/settings/pubkeys">
            SSH keys
          </Link>
          <Link className="rounded bg-blue-600 px-3 py-1.5 text-white" to="/devpods/new">
            New DevPod
          </Link>
        </nav>
      </header>
      {meQ.data && (
        <p className="mb-4 text-sm text-slate-500">
          {meQ.data.user} · {meQ.data.usage.devpods} pods ({meQ.data.usage.running} running)
          {meQ.data.usage.compute.cpu && ` · cpu ${meQ.data.usage.compute.cpu}/${meQ.data.quota.compute?.cpu ?? "∞"}`}
        </p>
      )}
      <ul className="divide-y rounded-xl border bg-white">
        {(podsQ.data?.items ?? []).map((dp: DevPod) => (
          <li key={dp.metadata.name} className="flex items-center justify-between p-4">
            <div>
              <Link className="font-medium text-blue-700 hover:underline" to={`/devpods/${dp.metadata.name}`}>
                {dp.metadata.name}
              </Link>
              <span
                className={`ml-3 rounded-full px-2 py-0.5 text-xs ${phaseColor[dp.status?.phase ?? "Pending"]}`}
              >
                {dp.status?.phase ?? "Pending"}
              </span>
            </div>
            <button
              className="rounded border px-3 py-1 text-sm hover:bg-slate-50"
              onClick={() => toggle.mutate({ name: dp.metadata.name, running: !dp.spec.running })}
            >
              {dp.spec.running ? "Hibernate" : "Wake"}
            </button>
          </li>
        ))}
        {podsQ.data?.items?.length === 0 && (
          <li className="p-8 text-center text-sm text-slate-400">No DevPods yet — create one.</li>
        )}
      </ul>
    </main>
  );
}
