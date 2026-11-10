import { createContext, useCallback, useContext, useEffect, useRef, useSyncExternalStore, type ReactNode } from "react";
import { LiveHub, type LiveRow, type LiveStatus, type StreamSpec } from "./LiveHub";

type Ctx = { hub: LiveHub | null; enabled: boolean };
const HubContext = createContext<Ctx>({ hub: null, enabled: false });

export function LiveHubProvider({ hub, enabled, children }: { hub: LiveHub; enabled: boolean; children: ReactNode }) {
  return <HubContext.Provider value={{ hub, enabled }}>{children}</HubContext.Provider>;
}

export const useLiveEnabled = () => useContext(HubContext).enabled;

/** LiveEnabled re-provides the surrounding hub with live data switched
 *  on or off (the shell turns it on when the deployment has telemetry). */
export function LiveEnabled({ enabled, children }: { enabled: boolean; children: ReactNode }) {
  const { hub } = useContext(HubContext);
  return <HubContext.Provider value={{ hub, enabled }}>{children}</HubContext.Provider>;
}

const noopSub = () => () => {};

/** useLiveRow keeps spec's stream open while mounted and re-renders only
 *  when this key's row changes. status is "off" when live data is
 *  disabled (no stream is opened). */
export function useLiveRow<T>(spec: StreamSpec | null, key: string): { row?: LiveRow<T>; status: LiveStatus } {
  const { hub, enabled } = useContext(HubContext);
  const active = !!(hub && enabled && spec);
  const id = spec?.id ?? "";
  const specRef = useRef(spec);
  specRef.current = spec;
  useEffect(() => {
    if (!active || !specRef.current) return;
    return hub!.acquire(specRef.current);
  }, [hub, active, id]);
  const subRow = useCallback((fn: () => void) => (active ? hub!.subscribeRow(id, key, fn) : () => {}), [hub, active, id, key]);
  const row = useSyncExternalStore(active ? subRow : noopSub, () => (active ? hub!.row<T>(id, key) : undefined));
  const subStatus = useCallback((fn: () => void) => (active ? hub!.subscribeStatus(fn) : () => {}), [hub, active]);
  const status = useSyncExternalStore(subStatus, () => (active ? hub!.status(id) : "off"));
  return { row, status };
}

/** useLiveStatus: whether any open stream is reconnecting (for the
 *  `实时数据重连中…` bar). */
export function useLiveStatus(): { reconnecting: boolean } {
  const { hub, enabled } = useContext(HubContext);
  const sub = useCallback((fn: () => void) => (hub && enabled ? hub.subscribeStatus(fn) : () => {}), [hub, enabled]);
  const reconnecting = useSyncExternalStore(sub, () => !!(hub && enabled && hub.anyReconnecting()));
  return { reconnecting };
}
