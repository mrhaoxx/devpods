// A reservation's details, for anyone who can see it (design 2026-11-24
// §1): the machine, who holds it, when, what of it (design 2026-11-26),
// its state and purpose. What can be
// done with it — change it, cancel or release it — only for one who may
// manage it: its holder, an admin, or one granted 管理预约 on its machine.
import * as Dialog from "@radix-ui/react-dialog";
import type { Reservation } from "../../data/api";
import { Button } from "../../design/Button";
import { fmtAmount, fmtSpan, fmtTime } from "../../lib/reservations";

export const phaseLabel: Record<Reservation["phase"], string> = { Upcoming: "即将开始", Active: "进行中", Ended: "已结束", Cancelled: "已取消" };

export function ReservationDetails({
  b,
  canManage,
  onClose,
  onEdit,
  onCancel,
}: {
  b: Reservation;
  canManage: boolean;
  onClose: () => void;
  onEdit: () => void;
  onCancel: () => void;
}) {
  const live = b.phase === "Upcoming" || b.phase === "Active";
  const row = (k: string, v: React.ReactNode) => (
    <div className="flex gap-3">
      <dt className="w-14 shrink-0 text-muted">{k}</dt>
      <dd className="min-w-0 break-words">{v}</dd>
    </div>
  );
  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content aria-describedby={undefined} className="fixed left-1/2 top-1/4 z-50 w-[min(92vw,420px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl">
          <Dialog.Title className="text-sm font-semibold">
            {b.node} · {b.mine ? "我的预约" : `${b.user} 的预约`}
          </Dialog.Title>
          <dl className="space-y-1.5 text-xs">
            {row("机器", <span className="mono">{b.cluster ? `${b.cluster} · ${b.node}` : b.node}</span>)}
            {row("预约人", b.user)}
            {row(
              "时间",
              <>
                {fmtTime(b.start)} – {fmtTime(b.end)}（{fmtSpan(Date.parse(b.end) - Date.parse(b.start))}）
              </>,
            )}
            {row("资源", fmtAmount(b))}
            {row("状态", phaseLabel[b.phase] + (b.phase === "Cancelled" && b.cancelledBy ? `，由 ${b.cancelledBy} 取消` : ""))}
            {row("用途", b.purpose ? b.purpose : <span className="text-faint">—</span>)}
          </dl>
          <div className="flex flex-wrap justify-end gap-2">
            {canManage && live && (
              <>
                <Button onClick={onEdit}>修改…</Button>
                <Button variant="danger" onClick={onCancel}>
                  {b.phase === "Active" ? "提前释放" : "取消预约"}
                </Button>
              </>
            )}
            <Dialog.Close asChild>
              <Button>关闭</Button>
            </Dialog.Close>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
