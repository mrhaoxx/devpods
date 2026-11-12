// The DevPod detail page's entry to snapshots: the 保存为镜像 button and,
// while one is being saved, a status that reopens its progress.
import type { Ref } from "../../data/cluster";
import { useState } from "react";
import type { SnapshotView } from "../../data/api";
import { useSnapshots } from "../../data/queries";
import { Button } from "../../design/Button";
import { SnapshotDialog } from "./SnapshotDialog";

export function SnapshotControl({ devpod, owner, running }: { devpod: Ref; owner: string; running: boolean }) {
  const q = useSnapshots({ devpod: devpod.name });
  const [open, setOpen] = useState<"new" | SnapshotView | null>(null);
  // the list names the DevPod; a same-named one in another cluster is not this one
  const busy = (q.data?.items ?? []).find((s) => (s.cluster ?? undefined) === devpod.cluster && (s.phase === "Pending" || s.phase === "Running"));
  return (
    <>
      {busy && (
        <button type="button" role="status" onClick={() => setOpen(busy)} className="inline-flex h-7 items-center gap-1.5 rounded-md border border-line bg-warm-soft px-2 text-xs text-ink hover:underline" title={busy.image}>
          <span aria-hidden>◐</span>
          正在保存镜像 · 查看进度
        </button>
      )}
      {running && !busy && (
        <Button onClick={() => setOpen("new")} title="把当前容器保存成镜像，之后可以用它新建 DevPod">
          保存为镜像
        </Button>
      )}
      {open && <SnapshotDialog devpod={devpod} owner={owner} following={open === "new" ? undefined : open} onClose={() => setOpen(null)} />}
    </>
  );
}
