import { useLiveStatus } from "../data/live/hooks";

/** A thin bar while a live stream is reconnecting (UI spec §6.3). */
export function ReconnectBar() {
  const { reconnecting } = useLiveStatus();
  if (!reconnecting) return null;
  return (
    <div role="status" className="bg-warm-soft px-3 py-0.5 text-center text-[11px] text-warm">
      实时数据重连中…
    </div>
  );
}
