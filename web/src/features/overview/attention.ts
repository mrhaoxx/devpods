// The direct action offered on each "需要处理" item (UI spec §5.3).
// Reservation kinds arrive with the reservation backend.
import { devpodHref } from "../../data/cluster";
import type { AttentionItem } from "../../data/api";

export type AttentionAction = { label: string; href?: string; op?: "hibernate" };

export function attentionAction(a: AttentionItem): AttentionAction | null {
  switch (a.kind) {
    case "idle_gpu":
      return { label: "休眠", op: "hibernate" };
    case "pending_long":
      return { label: "事件", href: devpodHref({ cluster: a.cluster, name: a.devpod ?? "" }, "events") };
    case "alert":
      return { label: "回放", href: `/audit/alerts?devpod=${a.devpod ?? ""}${a.cluster ? `&cluster=${encodeURIComponent(a.cluster)}` : ""}` };
    case "quota_high":
      return { label: "调整", href: `/admin/users?user=${a.owner ?? ""}` };
    default:
      return null;
  }
}

export const attentionTitle: Record<string, string> = {
  idle_gpu: "占着 GPU 空转",
  pending_long: "启动超过 10 分钟",
  alert: "审计告警",
  quota_high: "配额用到 90% 以上",
};
