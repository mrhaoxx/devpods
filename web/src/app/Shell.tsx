// The console layout (UI spec §3): sidebar + full-width content.
import { useState } from "react";
import { Outlet } from "react-router-dom";
import * as Dialog from "@radix-ui/react-dialog";
import { useMe } from "../data/queries";
import { LiveEnabled } from "../data/live/hooks";
import { Sidebar } from "./Sidebar";
import { Topbar } from "./Topbar";
import { ReconnectBar } from "./ReconnectBar";
import { EmptyState } from "../design/EmptyState";
import { CommandPalette } from "../features/search/CommandPalette";
import { ClusterGate, StaleBar } from "../features/clusters/ClusterSwitcher";
import type { Me } from "../data/api";

export function Shell() {
  const me = useMe();
  const [menu, setMenu] = useState(false);
  if (me.isError) return <EmptyState tone="error" title="无法加载账户信息" hint={String(me.error)} />;
  if (!me.data) return null;
  return (
    <ClusterGate>
      <ShellBody me={me.data} menu={menu} setMenu={setMenu} />
    </ClusterGate>
  );
}

function ShellBody({ me: data, menu, setMenu }: { me: Me; menu: boolean; setMenu: (o: boolean) => void }) {
  const me = { data };
  return (
    <LiveEnabled enabled={!!me.data.features.telemetry}>
      <div className="flex h-dvh overflow-hidden">
        <aside className="side w-[180px] shrink-0 border-r border-line bg-surface max-[1100px]:w-12 max-[700px]:hidden">
          <Sidebar me={me.data} />
        </aside>
        <Dialog.Root open={menu} onOpenChange={setMenu}>
          <Dialog.Portal>
            <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
            <Dialog.Content aria-describedby={undefined} className="fixed inset-y-0 left-0 z-50 w-[220px] border-r border-line bg-surface">
              <Dialog.Title className="sr-only">导航</Dialog.Title>
              <Sidebar me={me.data} onNavigate={() => setMenu(false)} />
            </Dialog.Content>
          </Dialog.Portal>
        </Dialog.Root>
        <div className="flex min-w-0 flex-1 flex-col">
          <ReconnectBar />
          <StaleBar />
          <Topbar me={me.data} onMenu={() => setMenu(true)} />
          <main className="relative min-h-0 flex-1 overflow-y-auto overflow-x-hidden">
            <Outlet />
          </main>
          <CommandPalette />
        </div>
      </div>
    </LiveEnabled>
  );
}
