// 我的 DevPod / 全部 DevPod (UI spec §5.1): one table, the admin view adds
// Owner, the 空转 / GPU chips and bulk actions.
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import * as Menu from "@radix-ui/react-dropdown-menu";
import type { SortingState } from "@tanstack/react-table";
import { deleteDevPod, patchDevPod, sshCommand, type LiveItem } from "../../data/api";
import { useDevPodMutations, useDevPods, useMe } from "../../data/queries";
import { devpodsStream } from "../../data/live/streams";
import { useLiveRow, useLiveValues } from "../../data/live/hooks";
import { DataTable } from "../../design/DataTable";
import { Chip } from "../../design/Chip";
import { Button } from "../../design/Button";
import { Input, Notice } from "../../design/Form";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { EmptyState } from "../../design/EmptyState";
import { copyText } from "../../design/Copy";
import { matchesFilter, parseFilter } from "../../lib/filter";
import { useUrlState, type Schema } from "../../lib/urlState";
import { runBulk, type BulkResult } from "../../lib/bulk";
import { useNow } from "../../lib/useNow";
import { TableCtx, devpodColumns, toRow, type Row } from "./columns";

const PHASES = ["Running", "Pending", "Stopped", "Failed"] as const;

const schema = {
  q: { kind: "string", default: "" },
  phase: { kind: "list", default: [] as string[] },
  sort: { kind: "string", default: "name" },
  idle: { kind: "bool", default: false },
  gpu: { kind: "bool", default: false },
} satisfies Schema;

const toSorting = (s: string): SortingState => (s ? [{ id: s.replace(/^-/, ""), desc: s.startsWith("-") }] : []);
const fromSorting = (s: SortingState) => (s[0] ? `${s[0].desc ? "-" : ""}${s[0].id}` : "");

type Outcome = { action: string; results: BulkResult<string>[] };

function RowMenu({ r, onDelete }: { r: Row; onDelete: (r: Row) => void }) {
  const me = useMe().data;
  const { setRunning } = useDevPodMutations();
  return (
    <Menu.Root>
      <Menu.Trigger aria-label={`${r.name} 操作`} className="rounded px-1.5 text-muted hover:bg-sunk hover:text-ink">
        ⋯
      </Menu.Trigger>
      <Menu.Portal>
        <Menu.Content align="end" className="z-50 min-w-36 rounded-md border border-line bg-surface p-1 text-xs shadow-lg">
          <Menu.Item className="cursor-pointer rounded px-2 py-1 outline-none data-[highlighted]:bg-sunk" onSelect={() => void copyText(sshCommand(me, r.owner, r.name))}>
            复制 SSH 命令
          </Menu.Item>
          <Menu.Item
            className="cursor-pointer rounded px-2 py-1 outline-none data-[highlighted]:bg-sunk"
            onSelect={() => setRunning.mutate({ name: r.name, running: !r.dp.spec.running })}
          >
            {r.dp.spec.running ? "休眠" : "启动"}
          </Menu.Item>
          <Menu.Item className="cursor-pointer rounded px-2 py-1 text-fail outline-none data-[highlighted]:bg-fail-soft" onSelect={() => onDelete(r)}>
            删除…
          </Menu.Item>
        </Menu.Content>
      </Menu.Portal>
    </Menu.Root>
  );
}

export function DevPodTable({ scope }: { scope: "mine" | "all" }) {
  const me = useMe().data;
  const admin = scope === "all" && !!me?.admin;
  const qc = useQueryClient();
  const list = useDevPods(scope);
  const [url, setUrl] = useUrlState(schema);
  const now = useNow(10_000);
  const spec = devpodsStream(scope);
  const { status } = useLiveRow(spec, "");
  const [confirm, setConfirm] = useState<{ rows: Row[]; bulk: boolean } | null>(null);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const { remove } = useDevPodMutations();

  const rows = useMemo(() => (list.data?.items ?? []).map((dp) => toRow(dp, list.data?.pods[dp.metadata.name])), [list.data]);
  const idle = useLiveValues<LiveItem>(spec, admin && url.idle ? rows.map((r) => r.name) : [], 5000, admin && url.idle);

  const filter = useMemo(() => parseFilter(url.q), [url.q]);
  const byQuery = useMemo(
    () =>
      rows.filter(
        (r) =>
          matchesFilter(r, filter) && (!admin || !url.gpu || r.gpu > 0) && (!admin || !url.idle || idle.get(r.name)?.idle === true),
      ),
    [rows, filter, admin, url.gpu, url.idle, idle],
  );
  const counts = Object.fromEntries(PHASES.map((p) => [p, byQuery.filter((r) => r.phase === p).length]));
  const shown = url.phase.length ? byQuery.filter((r) => url.phase.includes(r.phase)) : byQuery;

  const bulk = async (action: string, items: Row[], fn: (r: Row) => Promise<unknown>) => {
    const results = await runBulk(items, (r) => fn(r).then(() => undefined));
    setOutcome({ action, results: results.map((x) => ({ ...x, item: x.item.name })) });
    void qc.invalidateQueries({ queryKey: ["devpods"] });
  };
  const columns = useMemo(
    () => devpodColumns({ admin, me, actions: (r) => <RowMenu r={r} onDelete={(x) => setConfirm({ rows: [x], bulk: false })} /> }),
    [admin, me],
  );

  const failed = outcome?.results.filter((r) => !r.ok) ?? [];
  return (
    <TableCtx.Provider value={{ scope, now }}>
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <Input
          aria-label="过滤"
          placeholder="过滤：名称 user: node: phase: gpu>0"
          value={url.q}
          onChange={(e) => setUrl({ q: e.target.value })}
          className="mono !h-7 w-72 text-xs max-[700px]:w-full"
        />
        {PHASES.map((p) => (
          <Chip
            key={p}
            active={url.phase.includes(p)}
            count={counts[p]}
            onClick={() => setUrl({ phase: url.phase.includes(p) ? url.phase.filter((x) => x !== p) : [...url.phase, p] })}
          >
            {p}
          </Chip>
        ))}
        {admin && (
          <>
            <Chip active={url.idle} onClick={() => setUrl({ idle: !url.idle })}>
              空转 &gt;24h
            </Chip>
            <Chip active={url.gpu} onClick={() => setUrl({ gpu: !url.gpu })}>
              GPU only
            </Chip>
          </>
        )}
        <span className="flex-1" />
        {scope === "mine" && (
          <Link to="/?new=1" className="inline-flex h-7 items-center rounded-md bg-accent px-2.5 text-xs font-medium text-white hover:bg-accent-hi">
            + 新建 DevPod
          </Link>
        )}
      </div>
      {outcome && (
        <div className="flex items-start gap-2 border-b border-line px-3 py-1.5">
          <div className="flex-1">
            <Notice tone={failed.length ? "warm" : "run"}>
              {outcome.action}：成功 {outcome.results.length - failed.length}，失败 {failed.length}
              {failed.length > 0 && <>（{failed.map((f) => `${f.item}：${f.error}`).join("；")}）</>}
            </Notice>
          </div>
          <button type="button" onClick={() => setOutcome(null)} className="text-xs text-muted hover:text-ink" aria-label="关闭结果">
            ✕
          </button>
        </div>
      )}
      {list.isError ? (
        <EmptyState tone="error" title="无法加载 DevPod 列表" hint={String(list.error)} />
      ) : (
        <DataTable<Row>
          data={shown}
          columns={columns}
          getRowId={(r) => r.name}
          sorting={toSorting(url.sort)}
          onSortingChange={(s) => setUrl({ sort: fromSorting(s) })}
          selectable={admin}
          dim={status === "reconnecting"}
          empty={<EmptyState title={list.isLoading ? "加载中…" : rows.length ? "没有符合条件的 DevPod" : "还没有 DevPod"} />}
          bulkBar={(sel, clear) => (
            <>
              <Button onClick={() => void bulk("休眠", sel, (r) => patchDevPod(r.name, false)).then(clear)}>休眠</Button>
              <Button onClick={() => void bulk("启动", sel, (r) => patchDevPod(r.name, true)).then(clear)}>启动</Button>
              <Button variant="danger" onClick={() => setConfirm({ rows: sel, bulk: true })}>
                删除…
              </Button>
            </>
          )}
        />
      )}
      <ConfirmDialog
        open={!!confirm}
        onOpenChange={(o) => !o && setConfirm(null)}
        title={confirm?.bulk ? `删除 ${confirm.rows.length} 个 DevPod？` : `删除 ${confirm?.rows[0]?.name}？`}
        confirmLabel="确认删除"
        requireText={confirm?.bulk ? "删除" : undefined}
        danger
        onConfirm={() => {
          const rs = confirm!.rows;
          setConfirm(null);
          if (rs.length === 1 && !confirm!.bulk) remove.mutate(rs[0].name);
          else void bulk("删除", rs, (r) => deleteDevPod(r.name));
        }}
      >
        删除后 Pod 和持久化数据都会被移除，不能恢复。
      </ConfirmDialog>
    </TableCtx.Provider>
  );
}
