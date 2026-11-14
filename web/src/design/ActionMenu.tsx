// ActionMenu: a row's ⋯ — the same actions its right-click menu offers,
// worked out when it opens.
import * as Menu from "@radix-ui/react-dropdown-menu";
import type { RowAction } from "./DataTable";

export function ActionMenu({ label, actions }: { label: string; actions: () => RowAction[] }) {
  return (
    <Menu.Root>
      <Menu.Trigger aria-label={label} className="rounded px-1.5 text-muted hover:bg-sunk hover:text-ink">
        ⋯
      </Menu.Trigger>
      <Menu.Portal>
        <Menu.Content align="end" className="z-50 min-w-36 rounded-md border border-line bg-surface p-1 text-xs shadow-lg">
          {actions().map((a) => (
            <Menu.Item
              key={a.label}
              disabled={a.disabled}
              title={a.title}
              onSelect={a.onSelect}
              className={`cursor-pointer rounded px-2 py-1 outline-none data-[disabled]:cursor-default data-[disabled]:opacity-50 ${
                a.danger ? "text-fail data-[highlighted]:bg-fail-soft" : "data-[highlighted]:bg-sunk"
              }`}
            >
              {a.label}
            </Menu.Item>
          ))}
        </Menu.Content>
      </Menu.Portal>
    </Menu.Root>
  );
}
