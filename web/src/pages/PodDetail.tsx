import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  getDevPod,
  patchDevPod,
  deleteDevPod,
  me,
  sshCommand,
  watchDevPods,
  watchDevPodEvents,
  K8sEvent,
} from "../api";

export default function PodDetail() {
  const { name = "" } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["devpod", name], queryFn: () => getDevPod(name) });
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });

  // Live status: the owner's DevPod watch stream pushes phase/spec
  // changes; refetch this pod whenever an event names it.
  useEffect(
    () =>
      watchDevPods(
        (_type, dp) => {
          if (dp.metadata.name === name) qc.invalidateQueries({ queryKey: ["devpod", name] });
        },
        () => qc.invalidateQueries({ queryKey: ["devpod", name] }),
      ),
    [name, qc],
  );

  // Live events: the server replays the backlog on connect, then
  // streams updates; merge by uid so MODIFIED bumps counts in place.
  const [eventsByUID, setEventsByUID] = useState<Record<string, K8sEvent>>({});
  useEffect(() => {
    setEventsByUID({});
    return watchDevPodEvents(name, (type, ev) => {
      setEventsByUID((m) => {
        const next = { ...m };
        if (type === "DELETED") delete next[ev.metadata.uid];
        else next[ev.metadata.uid] = ev;
        return next;
      });
    });
  }, [name]);
  const events = useMemo(
    () =>
      Object.values(eventsByUID).sort((a, b) => (a.lastTimestamp ?? "").localeCompare(b.lastTimestamp ?? "")),
    [eventsByUID],
  );

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
        <dt className="text-slate-500">SSH</dt>
        <dd className="font-mono text-xs">
          {sshCommand(meQ.data, dp.spec.owner, dp.metadata.name.slice(dp.spec.owner.length + 1))}
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
        <h2 className="mb-2 font-medium">
          Events <span className="ml-1 align-middle text-[10px] text-green-600">● live</span>
        </h2>
        <ul className="space-y-1 font-mono text-xs text-slate-600">
          {events.map((e) => (
            <li key={e.metadata.uid}>
              {e.reason}
              {e.count && e.count > 1 ? ` (×${e.count})` : ""}: {e.message}
            </li>
          ))}
          {events.length === 0 && <li className="text-slate-400">none</li>}
        </ul>
      </section>
    </main>
  );
}
