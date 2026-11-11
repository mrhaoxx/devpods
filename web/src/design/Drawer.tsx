// Right-side drawer for viewing / editing one object without leaving the
// list (UI spec §3). Full screen below 700 px.
import * as Dialog from "@radix-ui/react-dialog";
import type { ReactNode } from "react";

export function Drawer({
  open,
  onOpenChange,
  title,
  actions,
  width = 560,
  children,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  actions?: ReactNode;
  width?: number;
  children: ReactNode;
}) {
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed inset-y-0 right-0 z-50 flex w-full flex-col border-l border-line bg-surface shadow-xl max-[700px]:!max-w-none"
          style={{ maxWidth: width }}
        >
          <header className="flex items-center gap-2 border-b border-line px-4 py-2.5">
            <Dialog.Title className="min-w-0 flex-1 truncate text-sm font-semibold">{title}</Dialog.Title>
            {actions}
            <Dialog.Close className="rounded px-1.5 text-muted hover:bg-sunk hover:text-ink" aria-label="关闭">
              ✕
            </Dialog.Close>
          </header>
          <div className="relative min-h-0 flex-1 overflow-y-auto p-4">{children}</div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
