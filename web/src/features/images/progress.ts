// The three steps a snapshot goes through, and where a progress event
// puts each of them (snapshot spec v2 §2.3).
import type { SnapshotProgress } from "../../data/api";

export const STEPS = [
  { id: "commit", label: "提交容器" },
  { id: "push", label: "推送镜像" },
  { id: "done", label: "完成" },
] as const;
export type StepState = "todo" | "active" | "done" | "failed";

/** stepStates: which steps are behind, current or ahead. On an error the
 *  step that was running (per failedAt, the last stage seen) fails. */
export function stepStates(stage: SnapshotProgress["stage"], failedAt?: SnapshotProgress["stage"]): StepState[] {
  const order = ["commit", "push", "done"];
  if (stage === "done") return ["done", "done", "done"];
  if (stage === "error") {
    const at = Math.max(0, order.indexOf(failedAt ?? "commit"));
    return order.map((_, i) => (i < at ? "done" : i === at ? "failed" : "todo"));
  }
  const at = order.indexOf(stage); // -1 while pending
  return order.map((_, i) => (i < at ? "done" : i === at ? "active" : "todo"));
}

/** fmtElapsed: 75 → "1:15". */
export function fmtElapsed(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

/** defaultSnapshotName: the DevPod's name without its owner prefix. */
export function defaultSnapshotName(devpod: string, owner: string): string {
  const n = devpod.startsWith(owner + "-") ? devpod.slice(owner.length + 1) : devpod;
  return n.toLowerCase().replace(/[^a-z0-9._-]/g, "-").replace(/^[^a-z0-9]+/, "") || "image";
}

export const NAME_RE = /^[a-z0-9][a-z0-9._-]*$/;
/** A full image reference with a tag (not a digest). */
export function validImageRef(ref: string): boolean {
  if (!/^[a-zA-Z0-9][a-zA-Z0-9._/:-]*$/.test(ref)) return false;
  const slash = ref.lastIndexOf("/");
  const last = ref.slice(slash + 1);
  const colon = last.lastIndexOf(":");
  return slash > 0 && colon > 0 && colon < last.length - 1;
}
