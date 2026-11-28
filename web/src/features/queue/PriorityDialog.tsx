// PriorityDialog: the priority a job, several, or a DevPod waiting in
// line is to have. Lowered always; raised at most to what one may give
// in its cluster (the job rules: GET /api/jobs/limits) — an admin past it.
import * as Dialog from "@radix-ui/react-dialog";
import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { jobLimits } from "../../data/api";
import { useMe } from "../../data/queries";
import { Button } from "../../design/Button";
import { Field, Input, Notice } from "../../design/Form";

export function PriorityDialog({
  title,
  current,
  cluster,
  confirmLabel = "设定",
  hint,
  onSubmit,
  onClose,
}: {
  title: string;
  /** the priority now (several: the lowest of theirs) */
  current: number;
  cluster?: string;
  confirmLabel?: string;
  hint?: string;
  onSubmit(priority: number): Promise<unknown>;
  onClose(): void;
}) {
  const admin = !!useMe().data?.admin;
  const limits = useQuery({ queryKey: ["job-limits", cluster ?? ""], queryFn: () => jobLimits(cluster), enabled: !admin });
  const cap = admin || limits.data?.unlimitedPriority ? undefined : limits.data?.maxPriority;
  const [value, setValue] = useState(String(current));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const n = Number(value);
  const whole = value.trim() !== "" && Number.isInteger(n) && n >= 0;
  const overCap = whole && cap !== undefined && n > cap && n > current;
  const ok = whole && !overCap && !busy;
  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await onSubmit(n);
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-1/3 z-50 w-[min(92vw,380px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl"
        >
          <Dialog.Title className="text-sm font-semibold">{title}</Dialog.Title>
          <Field
            label="优先级"
            hint={cap === undefined ? (admin ? "管理员：不受上限" : undefined) : `最高可设 ${cap}`}
            error={overCap ? `最高只能设到 ${cap}` : whole || value === "" ? undefined : "0 或更大的整数"}
          >
            <Input
              aria-label="优先级"
              type="number"
              min={0}
              value={value}
              onChange={(e) => setValue(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && ok && void submit()}
              autoFocus
            />
          </Field>
          <p className="text-[11px] text-muted">{hint ?? "数字大的先运行；同级按提交先后。调低随时可以，调高最多到你的上限。"}</p>
          {error && <Notice>{error}</Notice>}
          <div className="flex justify-end gap-2">
            <Dialog.Close asChild>
              <Button>取消</Button>
            </Dialog.Close>
            <Button variant="primary" disabled={!ok} onClick={() => void submit()}>
              {confirmLabel}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
