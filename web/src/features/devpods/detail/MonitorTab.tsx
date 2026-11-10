import { EmptyState } from "../../../design/EmptyState";
import type { DevPodSample } from "../../../data/api";

/** 监控 tab: charts land in Task 15. */
export function MonitorTab(_p: { name: string; history?: ReadonlyArray<{ ts: number; value: DevPodSample }> }) {
  return <EmptyState title="监控图表" hint="即将上线" />;
}
