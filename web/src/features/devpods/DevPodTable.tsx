// 我的 DevPod / 全部 DevPod (UI spec §5.1): one table, the admin view adds
// Owner, the 空转 / GPU chips and bulk actions.
import { useClusterFilter } from "../../data/clusterFilter";
import { useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import * as Menu from "@radix-ui/react-dropdown-menu";
import type { SortingState } from "@tanstack/react-table";
import { devpodHref, keyOf } from "../../data/cluster";
import { deleteDevPod, podKey, patchDevPod, patchDevPodSpec, rebuildFromConfig, redeployDevPod, sshCommand, type LiveItem, shellPath } from "../../data/api";
import { useDevPods, useMe, useRecent } from "../../data/queries";
import { devpodsStream } from "../../data/live/streams";
import { useLiveEnabled, useLiveRow, useLiveValues } from "../../data/live/hooks";
import { DataTable, type RowAction } from "../../design/DataTable";
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
import { TableCtx, devpodColumns, toRow, type Pending, type Row } from "./columns";
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
// what a DevPod is being brought to: shown as pending until it gets there
// (or fails, or two minutes pass); a failure stays a minute
type Want = "Running" | "Stopped" | "gone";
type PendingEntry = Pending & { want?: Want; since: number };
const START = { label: "启动", want: "Running" as const };
const STOP = { label: "休眠", want: "Stopped" as const };
const DELETE = { label: "删除", want: "gone" as const };

/** ActionMenu: a row's ⋯ — the same actions its right-click menu offers,
 *  worked out when it opens. */
function ActionMenu({ label, actions }: { label: string; actions: () => RowAction[] }) {
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

const running = (r: Row) => r.phase === "Running";

/** flat: a selection with each group row standing for its members */
const flat = (rows: Row[]): Row[] => {
  const seen = new Set<string>();
  // each DevPod once — by cluster and name: same-named ones of two clusters are two
  return rows.flatMap((r) => r.group?.members ?? [r]).filter((r) => !seen.has(keyOf(r)) && seen.add(keyOf(r)));
};

// The filter box keeps what is typed and hands it on once typing pauses:
// with a thousand rows, redrawing the table on every key lags. A query
// changed elsewhere (back, a link) shows in the box.
function FilterBox({ value, onChange }: { value: string; onChange: (q: string) => void }) {
  const [text, setText] = useState(value);
  const sent = useRef(value);
  const send = useRef(onChange);
  send.current = onChange;
  useEffect(() => {
    if (value !== sent.current) {
      sent.current = value;
      setText(value);
    }
  }, [value]);
  useEffect(() => {
    if (text === sent.current) return;
    const t = setTimeout(() => {
      sent.current = text;
      send.current(text);
    }, 200);
    return () => clearTimeout(t);
  }, [text]);
  return (
    <Input
      aria-label="过滤"
      placeholder="过滤：Running gpu>0 owner: node: image:"
      value={text}
      onChange={(e) => setText(e.target.value)}
      className="mono !h-7 w-72 text-xs max-[700px]:w-full"
    />
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
  const [progress, setProgress] = useState<{ action: string; done: number; failed: number; total: number } | null>(null);
  const [pending, setPending] = useState<ReadonlyMap<string, PendingEntry>>(() => new Map());
  const [dialog, setDialog] = useState<{ kind: BulkKind; rows: Row[]; clear: () => void } | null>(null);

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
          matchesFilter({ ...r, endpoint: r.dp.status?.endpoint }, filter) && (!admin || !url.gpu || r.gpu > 0) && (!admin || !url.idle || idle.get(keyOf(r))?.idle === true),
      ),
    [rows, filter, admin, url.gpu, url.idle, idle],
  );
  const counts = Object.fromEntries(PHASES.map((p) => [p, byQuery.filter((r) => r.phase === p).length]));
  const filtered = url.phase.length ? byQuery.filter((r) => url.phase.includes(r.phase)) : byQuery;
  const shown = useMemo(() => groupRows(filtered, open), [filtered, open]);

  // every action on DevPods — one or many — goes here: their rows say
  // what is being done until it is, the bar how far it has got
  const bulk = async (action: string, items: Row[], fn: (r: Row) => Promise<unknown>, how: { label: string; want?: Want } = { label: "更新" }) => {
    const since = Date.now();
    setPending((cur) => {
      const next = new Map(cur);
      for (const r of items) next.set(keyOf(r), { label: how.label, want: how.want, since });
      return next;
    });
    setOutcome(null);
    setProgress({ action, done: 0, failed: 0, total: items.length });
    const results = await runBulk(items, async (r) => {
      try {
        await fn(r);
      } catch (e) {
        const error = e instanceof Error ? e.message : String(e);
        setPending((cur) => new Map(cur).set(keyOf(r), { label: how.label, error, since: Date.now() }));
        setProgress((p) => p && { ...p, done: p.done + 1, failed: p.failed + 1 });
        throw e;
      }
      if (!how.want)
        setPending((cur) => {
          const next = new Map(cur);
          next.delete(keyOf(r));
          return next;
        });
      setProgress((p) => p && { ...p, done: p.done + 1 });
    });
    setProgress(null);
    setOutcome({ action, results: results.map((x) => ({ ...x, item: x.item.name })) });
    void qc.invalidateQueries({ queryKey: ["devpods"] });
  };
  // a pending row is done when its DevPod got where it was sent (or failed,
  // or is gone), or after two minutes; a failure is shown a minute
  const byKey = useMemo(() => new Map(rows.map((r) => [keyOf(r), r])), [rows]);
  useEffect(() => {
    const t = Date.now();
    setPending((cur) => {
      let next: Map<string, PendingEntry> | null = null;
      for (const [k, p] of cur) {
        const r = byKey.get(k);
        const done = p.error
          ? t - p.since > 60_000
          : p.want === "gone"
            ? !r
            : !!p.want && (!r || r.phase === p.want || r.phase === "Failed" || t - p.since > 120_000);
        if (done) (next ??= new Map(cur)).delete(k);
      }
      return next ?? cur;
    });
  }, [byKey, now]);
  const busy = (r: Row) => {
    const p = pending.get(keyOf(r));
    return !!p && !p.error;
  };
  // what can be done to a DevPod, a group of them, a selection: said once,
  // offered by the ⋯ menu, the right-click menu and the bulk bar alike
  const devpodActions = (r: Row): RowAction[] => [
    { label: "在新标签页打开", onSelect: () => window.open(devpodHref(r), "_blank", "noopener") },
    { label: "复制 SSH 命令", onSelect: () => void copyText(sshCommand(me, r.owner, r.name, r.cluster)) },
    ...(me?.features.webshell && r.phase === "Running" ? [{ label: "终端", onSelect: () => window.open(shellPath(r), "_blank", "noopener") }] : []),
    r.dp.spec.running
      ? { label: "休眠", disabled: busy(r), onSelect: () => void bulk("休眠", [r], (x) => patchDevPod(x, false), STOP) }
      : { label: "启动", disabled: busy(r), onSelect: () => void bulk("启动", [r], (x) => patchDevPod(x, true), START) },
    { label: "删除…", danger: true, disabled: busy(r), onSelect: () => setConfirm({ rows: [r], bulk: false }) },
  ];
  const groupActions = (r: Row): RowAction[] => [
    { label: "整组启动", onSelect: () => void bulk("整组启动", r.group!.members, (m) => patchDevPod(m, true), START) },
    { label: "整组休眠", onSelect: () => void bulk("整组休眠", r.group!.members, (m) => patchDevPod(m, false), STOP) },
    { label: "按配置重建整组…", onSelect: () => setRebuildAsk(r) },
    { label: "删除整组…", danger: true, onSelect: () => setConfirm({ rows: r.group!.members, bulk: true }) },
  ];
  const rowActions = (r: Row) => (r.group ? groupActions(r) : devpodActions(r));
  // the ⋯ menu is in the (memoised) columns: it asks for what is current
  const rowActionsNow = useRef(rowActions);
  rowActionsNow.current = rowActions;
  const bulkActions = (picked: Row[], clear: () => void): RowAction[] => {
    const sel = flat(picked);
    // nodes are a cluster's: DevPods of two clusters cannot move to the same ones
    const acrossClusters = new Set(sel.map((r) => r.cluster ?? "")).size > 1;
    return [
      {
        label: "迁移…",
        disabled: acrossClusters,
        title: acrossClusters ? "所选 DevPod 分属不同集群，不能一起迁移：只选同一个集群的" : undefined,
        onSelect: () => setDialog({ kind: "migrate", rows: sel, clear }),
      },
      { label: "休眠", onSelect: () => void bulk("休眠", sel, (r) => patchDevPod(r, false), STOP).then(clear) },
      { label: "启动", onSelect: () => void bulk("启动", sel, (r) => patchDevPod(r, true), START).then(clear) },
      { label: "协作者…", onSelect: () => setDialog({ kind: "collaborators", rows: sel, clear }) },
      { label: "设置镜像…", onSelect: () => setDialog({ kind: "image", rows: sel, clear }) },
      { label: "重新部署…", onSelect: () => setDialog({ kind: "redeploy", rows: sel, clear }) },
      { label: "删除…", danger: true, onSelect: () => setConfirm({ rows: sel, bulk: true }) },
    ];
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
        actions: (r) => <ActionMenu label={`${r.name} 操作`} actions={() => rowActionsNow.current(r)} />,
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [admin, me, open, used, several, clusters],
  );

  const failed = outcome?.results.filter((r) => !r.ok) ?? [];
  return (
    <TableCtx.Provider value={{ scope, now, recent: recent.data, pending }}>
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <FilterBox value={url.q} onChange={(q) => setUrl({ q })} />
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
      {progress && (
        <div className="border-b border-line px-3 py-1.5">
          <Notice tone="idle">
            {progress.action}：进行中 {progress.done}/{progress.total}…{progress.failed > 0 && `（失败 ${progress.failed}）`}
          </Notice>
        </div>
      )}
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
          bulkBar={(picked, clear) =>
            bulkActions(picked, clear).map((a) => (
              <Button key={a.label} variant={a.danger ? "danger" : undefined} disabled={a.disabled} title={a.title} onClick={a.onSelect}>
                {a.label}
              </Button>
            ))
          }
          rowMenu={(picked, isBulk, clear) => (isBulk ? bulkActions(picked, clear) : rowActions(picked[0]))}
        />
      )}
      <BulkDialog
        key={dialog?.kind ?? "none"}
        kind={dialog?.kind ?? null}
        targets={(dialog?.rows ?? []).map((r) => ({ cluster: r.cluster, name: r.name, collaborators: r.dp.spec.collaborators ?? [] }))}
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
          void bulk("删除", rs, (r) => deleteDevPod(r), DELETE);
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
          void bulk("按配置重建", g.group!.members, (m) => rebuildFromConfig(m), { label: "重建" });
        }}
      >
        用来源配置现在的内容重建每个 DevPod。运行中的会重建 Pod，已有的会话会断开；home 卷不变。
      </ConfirmDialog>
    </TableCtx.Provider>
  );
}
