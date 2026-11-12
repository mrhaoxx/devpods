// 我的 DevPod / 全部 DevPod (UI spec §5.1): one table, the admin view adds
// Owner, the 空转 / GPU chips and bulk actions.
import { useClusterFilter } from "../../data/clusterFilter";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import * as Menu from "@radix-ui/react-dropdown-menu";
import type { SortingState } from "@tanstack/react-table";
import { keyOf } from "../../data/cluster";
import { deleteDevPod, podKey, patchDevPod, patchDevPodSpec, rebuildFromConfig, redeployDevPod, sshCommand, type LiveItem, shellPath } from "../../data/api";
import { useDevPodMutations, useDevPods, useMe, useRecent } from "../../data/queries";
import { devpodsStream } from "../../data/live/streams";
import { useLiveEnabled, useLiveRow, useLiveValues } from "../../data/live/hooks";
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
import { BulkDialog, type BulkKind } from "./BulkDialog";
import { useNow } from "../../lib/useNow";
import { TableCtx, devpodColumns, toRow, type Row } from "./columns";
import { groupRows } from "./groups";
import { groupUsage, usage, usageKeys, type UsageKey } from "./usage";

const PHASES = ["Running", "Pending", "Stopped", "Failed"] as const;

const schema = {
  q: { kind: "string", default: "" },
  phase: { kind: "list", default: [] as string[] },
  sort: { kind: "string", default: "name" },
  idle: { kind: "bool", default: false },
  gpu: { kind: "bool", default: false },
  /** a group shown open (a launch of several lands here) */
  group: { kind: "string", default: "" },
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
          <Menu.Item className="cursor-pointer rounded px-2 py-1 outline-none data-[highlighted]:bg-sunk" onSelect={() => void copyText(sshCommand(me, r.owner, r.name, r.cluster))}>
            复制 SSH 命令
          </Menu.Item>
          {me?.features.webshell && r.phase === "Running" && (
            <Menu.Item className="cursor-pointer rounded px-2 py-1 outline-none data-[highlighted]:bg-sunk" onSelect={() => window.open(shellPath(r), "_blank", "noopener")}>
              终端
            </Menu.Item>
          )}
          <Menu.Item
            className="cursor-pointer rounded px-2 py-1 outline-none data-[highlighted]:bg-sunk"
            onSelect={() => setRunning.mutate({ ref: r, running: !r.dp.spec.running })}
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

const item = "cursor-pointer rounded px-2 py-1 outline-none data-[highlighted]:bg-sunk";

/** GroupMenu: what is done to every DevPod of a group at once. */
function GroupMenu({ r, onRun, onAsk }: { r: Row; onRun: (action: string, fn: (m: Row) => Promise<unknown>) => void; onAsk: (what: "delete" | "rebuild") => void }) {
  return (
    <Menu.Root>
      <Menu.Trigger aria-label={`${r.name} 操作`} className="rounded px-1.5 text-muted hover:bg-sunk hover:text-ink">
        ⋯
      </Menu.Trigger>
      <Menu.Portal>
        <Menu.Content align="end" className="z-50 min-w-36 rounded-md border border-line bg-surface p-1 text-xs shadow-lg">
          <Menu.Item className={item} onSelect={() => onRun("整组启动", (m) => patchDevPod(m, true))}>
            整组启动
          </Menu.Item>
          <Menu.Item className={item} onSelect={() => onRun("整组休眠", (m) => patchDevPod(m, false))}>
            整组休眠
          </Menu.Item>
          <Menu.Item className={item} onSelect={() => onAsk("rebuild")}>
            按配置重建整组…
          </Menu.Item>
          <Menu.Item className="cursor-pointer rounded px-2 py-1 text-fail outline-none data-[highlighted]:bg-fail-soft" onSelect={() => onAsk("delete")}>
            删除整组…
          </Menu.Item>
        </Menu.Content>
      </Menu.Portal>
    </Menu.Root>
  );
}

const running = (r: Row) => r.phase === "Running";

/** flat: a selection with each group row standing for its members */
const flat = (rows: Row[]): Row[] => {
  const seen = new Set<string>();
  return rows.flatMap((r) => r.group?.members ?? [r]).filter((r) => !seen.has(r.name) && seen.add(r.name));
};

export function DevPodTable({ scope }: { scope: "mine" | "all" }) {
  const me = useMe().data;
  const admin = scope === "all" && !!me?.admin;
  const qc = useQueryClient();
  const list = useDevPods(scope);
  const [url, setUrl] = useUrlState(schema);
  const now = useNow(10_000);
  const spec = devpodsStream(scope);
  const { status } = useLiveRow(spec, "");
  const recent = useRecent({ scope }, useLiveEnabled());
  const [confirm, setConfirm] = useState<{ rows: Row[]; bulk: boolean } | null>(null);
  const [rebuildAsk, setRebuildAsk] = useState<Row | null>(null);
  const [open, setOpen] = useState<ReadonlySet<string>>(() => new Set(url.group ? [url.group] : []));
  const toggle = (id: string) =>
    setOpen((cur) => {
      const next = new Set(cur);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const [dialog, setDialog] = useState<{ kind: BulkKind; rows: Row[]; clear: () => void } | null>(null);
  const { remove } = useDevPodMutations();

  const { matches, several, clusters } = useClusterFilter();
  const rows = useMemo(
    () => (list.data?.items ?? []).filter(matches).map((dp) => toRow(dp, list.data?.pods[podKey(dp)])),
    [list.data, matches],
  );
  const idle = useLiveValues<LiveItem>(spec, admin && url.idle ? rows.map(keyOf) : [], 5000, admin && url.idle);
  // sorting by what DevPods use: their values, taken every 5 s so the rows do not jump every second
  const byUsage = usageKeys.includes(url.sort.replace(/^-/, "") as UsageKey);
  const used = useLiveValues<LiveItem>(spec, byUsage ? rows.map(keyOf) : [], 5000, byUsage);

  const filter = useMemo(() => parseFilter(url.q), [url.q]);
  const byQuery = useMemo(
    () =>
      rows.filter(
        (r) =>
          matchesFilter(r, filter) && (!admin || !url.gpu || r.gpu > 0) && (!admin || !url.idle || idle.get(keyOf(r))?.idle === true),
      ),
    [rows, filter, admin, url.gpu, url.idle, idle],
  );
  const counts = Object.fromEntries(PHASES.map((p) => [p, byQuery.filter((r) => r.phase === p).length]));
  const filtered = url.phase.length ? byQuery.filter((r) => url.phase.includes(r.phase)) : byQuery;
  const shown = useMemo(() => groupRows(filtered, open), [filtered, open]);

  const bulk = async (action: string, items: Row[], fn: (r: Row) => Promise<unknown>) => {
    const results = await runBulk(items, (r) => fn(r).then(() => undefined));
    setOutcome({ action, results: results.map((x) => ({ ...x, item: x.item.name })) });
    void qc.invalidateQueries({ queryKey: ["devpods"] });
  };
  const columns = useMemo(
    () =>
      devpodColumns({
        clusters: several ? clusters : undefined,
        admin,
        me,
        groups: { open, toggle },
        usage: (r, key) =>
          r.group ? groupUsage(r.group.members.map((m) => (running(m) ? used.get(keyOf(m)) : undefined)), key) : running(r) ? usage(used.get(keyOf(r)), key) : undefined,
        actions: (r) =>
          r.group ? (
            <GroupMenu
              r={r}
              onRun={(action, fn) => void bulk(action, r.group!.members, fn)}
              onAsk={(what) => (what === "delete" ? setConfirm({ rows: r.group!.members, bulk: true }) : setRebuildAsk(r))}
            />
          ) : (
            <RowMenu r={r} onDelete={(x) => setConfirm({ rows: [x], bulk: false })} />
          ),
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [admin, me, open, used, several, clusters],
  );

  const failed = outcome?.results.filter((r) => !r.ok) ?? [];
  return (
    <TableCtx.Provider value={{ scope, now, recent: recent.data }}>
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
          storageKey="devpods"
          data={shown}
          columns={columns}
          getRowId={(r) => (r.group ? `group:${keyOf(r)}` : keyOf(r))}
          sorting={toSorting(url.sort)}
          onSortingChange={(s) => setUrl({ sort: fromSorting(s) })}
          selectable
          dim={status === "reconnecting"}
          empty={<EmptyState title={list.isLoading ? "加载中…" : rows.length ? "没有符合条件的 DevPod" : "还没有 DevPod"} />}
          bulkBar={(picked, clear) => {
            const sel = flat(picked);
            return (
            <>
              <Button onClick={() => setDialog({ kind: "migrate", rows: sel, clear })}>迁移…</Button>
              <Button onClick={() => void bulk("休眠", sel, (r) => patchDevPod(r, false)).then(clear)}>休眠</Button>
              <Button onClick={() => void bulk("启动", sel, (r) => patchDevPod(r, true)).then(clear)}>启动</Button>
              <Button onClick={() => setDialog({ kind: "collaborators", rows: sel, clear })}>协作者…</Button>
              <Button onClick={() => setDialog({ kind: "image", rows: sel, clear })}>设置镜像…</Button>
              <Button onClick={() => setDialog({ kind: "redeploy", rows: sel, clear })}>重新部署…</Button>
              <Button variant="danger" onClick={() => setConfirm({ rows: sel, bulk: true })}>
                删除…
              </Button>
            </>
            );
          }}
        />
      )}
      <BulkDialog
        key={dialog?.kind ?? "none"}
        kind={dialog?.kind ?? null}
        targets={(dialog?.rows ?? []).map((r) => ({ name: r.name, collaborators: r.dp.spec.collaborators ?? [] }))}
        onClose={() => setDialog(null)}
        onRun={(plan) => {
          const d = dialog!;
          setDialog(null);
          const index = new Map(d.rows.map((r, i) => [keyOf(r), i]));
          void bulk(plan.action, d.rows, (r) =>
            plan.redeploy ? redeployDevPod(r) : patchDevPodSpec(r, plan.patch!({ name: r.name, collaborators: r.dp.spec.collaborators ?? [] }, index.get(keyOf(r))!)),
          ).then(d.clear);
        }}
      />
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
          if (rs.length === 1 && !confirm!.bulk) remove.mutate(rs[0]);
          else void bulk("删除", rs, (r) => deleteDevPod(r));
        }}
      >
        删除后 Pod 和持久化数据都会被移除，不能恢复。
      </ConfirmDialog>
      <ConfirmDialog
        open={!!rebuildAsk}
        onOpenChange={(o) => !o && setRebuildAsk(null)}
        title={`按配置重建 ${rebuildAsk?.name} 的 ${rebuildAsk?.group?.members.length ?? 0} 个 DevPod？`}
        confirmLabel="重建"
        onConfirm={() => {
          const g = rebuildAsk!;
          setRebuildAsk(null);
          void bulk("按配置重建", g.group!.members, (m) => rebuildFromConfig(m));
        }}
      >
        用来源配置现在的内容重建每个 DevPod。运行中的会重建 Pod，已有的会话会断开；home 卷不变。
      </ConfirmDialog>
    </TableCtx.Provider>
  );
}
