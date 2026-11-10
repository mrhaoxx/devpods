// A centered confirmation; with requireText the confirm button stays
// disabled until that exact text is typed (bulk delete, UI spec §5.1).
import * as Dialog from "@radix-ui/react-dialog";
import { useState, type ReactNode } from "react";
import { Button } from "./Button";
import { Input } from "./Form";

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  children,
  confirmLabel,
  requireText,
  danger,
  onConfirm,
}: {
  open: boolean;
  onOpenChange: (o: boolean) => void;
  title: ReactNode;
  children?: ReactNode;
  confirmLabel: string;
  requireText?: string;
  danger?: boolean;
  onConfirm: () => void;
}) {
  const [typed, setTyped] = useState("");
  const ok = !requireText || typed === requireText;
  return (
    <Dialog.Root
      open={open}
      onOpenChange={(o) => {
        if (!o) setTyped("");
        onOpenChange(o);
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-1/3 z-50 w-[min(92vw,420px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl"
        >
          <Dialog.Title className="text-sm font-semibold">{title}</Dialog.Title>
          {children && <div className="text-xs text-muted">{children}</div>}
          {requireText && (
            <label className="block text-xs">
              <span className="mb-1 block text-muted">输入 {requireText} 以确认</span>
              <Input aria-label={`输入 ${requireText} 以确认`} value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
            </label>
          )}
          <div className="flex justify-end gap-2">
            <Dialog.Close asChild>
              <Button>取消</Button>
            </Dialog.Close>
            <Button
              variant={danger ? "danger" : "primary"}
              disabled={!ok}
              onClick={() => {
                setTyped("");
                onConfirm();
              }}
            >
              {confirmLabel}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
