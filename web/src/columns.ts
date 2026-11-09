// Column model of the DevPod table: which columns exist and which ones
// a narrow screen drops first (spec §9.4: Image → RDMA → Net → Up).

export const ALL_COLUMNS = [
  "name",
  "phase",
  "cpu",
  "memory",
  "gpu",
  "net",
  "rdma",
  "sess",
  "up",
  "image",
  "ssh",
  "actions",
] as const;

export type ColumnKey = (typeof ALL_COLUMNS)[number];

export const HIDE_ORDER: ColumnKey[] = ["image", "rdma", "net", "up"];
// HIDE_ORDER[i] disappears below BREAKPOINTS[i] pixels.
const BREAKPOINTS = [1100, 980, 860, 760];

export function visibleColumns(width: number, available: readonly ColumnKey[]): ColumnKey[] {
  const hidden = new Set(HIDE_ORDER.filter((_, i) => width < BREAKPOINTS[i]));
  return available.filter((c) => !hidden.has(c));
}
