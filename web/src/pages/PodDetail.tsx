import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { getDevPod, getEvents, patchDevPod, deleteDevPod } from "../api";

export default function PodDetail() {
  const { name = "" } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["devpod", name], queryFn: () => getDevPod(name), refetchInterval: 5000 });
  const ev = useQuery({ queryKey: ["events", name], queryFn: () => getEvents(name), refetchInterval: 10000 });

  const toggle = useMutation({
    mutationFn: (running: boolean) => patchDevPod(name, running),
    onSettled: () => qc.invalidateQueries({ queryKey: ["devpod", name] }),
  });
  const del = useMutation({
    mutationFn: () => deleteDevPod(name),
    onSuccess: () => nav("/"),
  });

  if (!q.data) return <main className="p-8 text-sm text-slate-400">Loading…</main>;
  const dp = q.data.devpod;
  const binding = q.data.binding;

  return (
    <main className="mx-auto max-w-3xl p-8">
      <Link to="/" className="text-sm text-blue-600 hover:underline">
        ← My DevPods
      </Link>
      <header className="mb-6 mt-2 flex items-center justify-between">
        <h1 className="text-xl font-semibold">{dp.metadata.name}</h1>
        <div className="flex gap-2">
          <button className="rounded border px-3 py-1.5 text-sm" onClick={() => toggle.mutate(!dp.spec.running)}>
            {dp.spec.running ? "Hibernate" : "Wake"}
          </button>
          <button
            className="rounded border border-red-300 px-3 py-1.5 text-sm text-red-700"
            onClick={() => {
              if (confirm(`Delete ${dp.metadata.name}? PVC data is lost.`)) del.mutate();
            }}
          >
            Delete
          </button>
        </div>
      </header>

      <dl className="mb-6 grid grid-cols-2 gap-x-8 gap-y-2 rounded-xl border bg-white p-4 text-sm">
        <dt className="text-slate-500">Phase</dt>
        <dd>{dp.status?.phase ?? "Pending"}</dd>
        <dt className="text-slate-500">Endpoint</dt>
        <dd className="font-mono">{dp.status?.endpoint ?? "—"}</dd>
        <dt className="text-slate-500">SSH</dt>
        <dd className="font-mono text-xs">
          ssh {dp.spec.owner}+{dp.metadata.name.slice(dp.spec.owner.length + 1)}@&lt;gateway&gt;
        </dd>
        {dp.status?.message && (
          <>
            <dt className="text-slate-500">Message</dt>
            <dd className="text-red-700">{dp.status.message}</dd>
          </>
        )}
      </dl>

      {binding && (
        <section className="mb-6 rounded-xl border bg-white p-4 text-sm">
          <h2 className="mb-2 font-medium">CPU binding (Kore)</h2>
          <dl className="grid grid-cols-2 gap-x-8 gap-y-2">
            {binding.allocatedCpuset && (
              <>
                <dt className="text-slate-500">Allocated cores</dt>
                <dd className="font-mono">{binding.allocatedCpuset}</dd>
              </>
            )}
            {binding.reservedNuma && (
              <>
                <dt className="text-slate-500">NUMA zone</dt>
                <dd>{binding.reservedNuma}</dd>
              </>
            )}
            {binding.pool && (
              <>
                <dt className="text-slate-500">Pool</dt>
                <dd>
                  {binding.pool} ({binding.poolSize} cores)
                </dd>
              </>
            )}
            {!binding.allocatedCpuset && !binding.pool && (
              <>
                <dt className="text-slate-500">State</dt>
                <dd>binding pending</dd>
              </>
            )}
          </dl>
        </section>
      )}

      <section className="rounded-xl border bg-white p-4 text-sm">
        <h2 className="mb-2 font-medium">Events</h2>
        <ul className="space-y-1 font-mono text-xs text-slate-600">
          {((ev.data?.items ?? []) as { reason?: string; message?: string }[]).map((e, i) => (
            <li key={i}>
              {e.reason}: {e.message}
            </li>
          ))}
          {ev.data?.items?.length === 0 && <li className="text-slate-400">none</li>}
        </ul>
      </section>
    </main>
  );
}
