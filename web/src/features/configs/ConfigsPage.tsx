// 配置 (config center spec §6.1): saved DevPod YAML with variables — my
// own, those shared with me, and the public ones. Any of them can be
// launched or copied; only my own can be changed.
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import { ApiFailure } from "../../data/api";
import { useMe } from "../../data/queries";
import { Badge } from "../../design/Badge";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { EmptyState } from "../../design/EmptyState";
import { Notice } from "../../design/Form";
import { fmtAge } from "../../lib/format";
import { deleteConfig, getConfig, listConfigs, patchConfig, type ConfigScope, type ConfigSummary } from "./api";

export const configsKey = (scope: ConfigScope) => ["configs", scope] as const;
const useConfigs = (scope: ConfigScope) => useQuery({ queryKey: configsKey(scope), queryFn: () => listConfigs(scope), retry: false });

function Group({
  title,
  empty,
  items,
  showOwner,
  admin,
  onCopy,
  onDelete,
  onUnpublish,
  onOfficial,
}: {
  title: string;
  empty: string;
  items: ConfigSummary[];
  showOwner: boolean;
  admin: boolean;
  onCopy: (c: ConfigSummary) => void;
  onDelete: (c: ConfigSummary) => void;
  onUnpublish: (c: ConfigSummary) => void;
  onOfficial: (c: ConfigSummary, official: boolean) => void;
}) {
  return (
    <section aria-label={title} className="rounded-lg border border-line bg-surface">
      <header className="border-b border-line px-3 py-2">
        <h2 className="text-xs font-semibold text-muted">
          {title} <span className="mono font-normal text-faint">{items.length}</span>
        </h2>
      </header>
      {items.length === 0 ? (
        <p className="px-3 py-4 text-xs text-faint">{empty}</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full border-collapse text-xs">
            <thead>
              <tr className="h-7 border-b border-line text-left text-[11px] text-muted">
                <th className="px-3 font-normal">配置</th>
                {showOwner && <th className="px-2 font-normal">所有者</th>}
                <th className="px-2 font-normal">变量</th>
                <th className="px-2 font-normal">更新</th>
                <th className="px-3" />
              </tr>
            </thead>
            <tbody>
              {items.map((c) => (
                <tr key={c.name} className="border-b border-line align-top last:border-0">
                  <td className="max-w-[26rem] px-3 py-1.5">
                    <div className="flex flex-wrap items-center gap-1.5">
                      <Link to={`/configs/${c.name}`} className="font-medium text-ink hover:text-accent">
                        {c.title}
                      </Link>
                      {c.official && <Badge tone="accent">官方</Badge>}
                      {c.public && !c.official && <Badge tone="accent">公开</Badge>}
                      {c.mine && c.sharedWith.length > 0 && <Badge title={c.sharedWith.join(", ")}>分享给 {c.sharedWith.length} 人</Badge>}
                    </div>
                    <div className="mono text-[11px] text-faint">{c.name}</div>
                    {c.description && <div className="text-[11px] text-muted">{c.description}</div>}
                  </td>
                  {showOwner && <td className="mono px-2 py-1.5">{c.owner}</td>}
                  <td className="px-2 py-1.5">
                    <span className="flex flex-wrap gap-1">
                      {c.variables.length === 0 && <span className="text-faint">—</span>}
                      {c.variables.map((v) => (
                        <span key={v.name} className="mono rounded bg-sunk px-1 text-[11px] text-muted" title={v.default ? `默认 ${v.default}` : undefined}>
                          {v.name}
                        </span>
                      ))}
                    </span>
                  </td>
                  <td className="px-2 py-1.5 whitespace-nowrap" title={new Date(c.updatedAt).toLocaleString()}>
                    {fmtAge(c.updatedAt)} 前
                  </td>
                  <td className="px-3 py-1.5 text-right whitespace-nowrap">
                    <span className="inline-flex gap-3">
                      <Link to={`/?new=1&config=${encodeURIComponent(c.name)}`} className="text-accent hover:underline">
                        用此配置创建
                      </Link>
                      {c.mine && (
                        <Link to={`/configs/${c.name}`} className="text-muted hover:text-accent">
                          编辑
                        </Link>
                      )}
                      <button type="button" onClick={() => onCopy(c)} className="text-muted hover:text-accent">
                        另存为副本
                      </button>
                      {admin && c.public && (
                        <button type="button" onClick={() => onOfficial(c, !c.official)} className="text-muted hover:text-accent">
                          {c.official ? "取消官方" : "设为官方"}
                        </button>
                      )}
                      {!c.mine && admin && c.public && (
                        <button type="button" onClick={() => onUnpublish(c)} className="text-muted hover:text-fail">
                          取消公开
                        </button>
                      )}
                      {(c.mine || admin) && (
                        <button type="button" onClick={() => onDelete(c)} className="text-muted hover:text-fail" aria-label={`删除 ${c.title}`}>
                          删除
                        </button>
                      )}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export function ConfigsPage() {
  const me = useMe().data;
  const admin = !!me?.admin;
  const nav = useNavigate();
  const qc = useQueryClient();
  const mine = useConfigs("mine");
  const shared = useConfigs("shared");
  const pub = useConfigs("public");
  const off = useConfigs("official");
  const [forget, setForget] = useState<ConfigSummary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const refresh = () => void qc.invalidateQueries({ queryKey: ["configs"] });
  const say = (e: unknown) => setError(e instanceof ApiFailure ? e.body.message : String(e));
  const del = useMutation({ mutationFn: (name: string) => deleteConfig(name), onError: say, onSettled: refresh });
  const unpublish = useMutation({ mutationFn: (name: string) => patchConfig(name, { public: false }), onError: say, onSettled: refresh });
  const mark = useMutation({ mutationFn: (p: { name: string; official: boolean }) => patchConfig(p.name, { official: p.official }), onError: say, onSettled: refresh });
  // a copy starts a new config of mine from the other's text
  const copy = (c: ConfigSummary) =>
    getConfig(c.name)
      .then((full) => nav("/configs/new", { state: { content: full.content, title: full.title, description: full.description } }))
      .catch(say);

  // a config is listed once: official first, then mine, shared, public
  const of = off.data?.items ?? [];
  const isOff = new Set(of.map((c) => c.name));
  const my = (mine.data?.items ?? []).filter((c) => !isOff.has(c.name));
  const sh = (shared.data?.items ?? []).filter((c) => !isOff.has(c.name));
  const seen = new Set([...of, ...my, ...sh].map((c) => c.name));
  const pu = (pub.data?.items ?? []).filter((c) => !seen.has(c.name));
  const loading = mine.isLoading || shared.isLoading || pub.isLoading;
  const failed = mine.isError && shared.isError && pub.isError;
  const none = !loading && !failed && of.length + my.length + sh.length + pu.length === 0;
  const act = {
    admin,
    onCopy: (c: ConfigSummary) => void copy(c),
    onDelete: setForget,
    onUnpublish: (c: ConfigSummary) => unpublish.mutate(c.name),
    onOfficial: (c: ConfigSummary, official: boolean) => mark.mutate({ name: c.name, official }),
  };

  return (
    <div className="space-y-3 p-3">
      <header className="flex flex-wrap items-center gap-2">
        <h1 className="text-sm font-semibold">配置</h1>
        <span className="flex-1" />
        <Button variant="primary" onClick={() => nav("/configs/new")}>
          + 新建配置
        </Button>
      </header>
      <p className="text-xs text-muted">
        把一份带变量的 DevPod YAML 存成配置，之后选配置、填变量就能创建，一次可以创建多个。配置可以分享给指定用户或全站公开；别人用你的配置创建时，按他自己的权限和配额检查。
      </p>
      {error && <Notice>{error}</Notice>}
      {failed ? (
        <EmptyState tone="error" title="加载失败" />
      ) : loading ? (
        <EmptyState title="加载中…" />
      ) : none ? (
        <EmptyState title="还没有保存过配置" hint="点「新建配置」，或在 DevPod 详情页点「存为配置」。" />
      ) : (
        <>
          {of.length > 0 && <Group title="官方" empty="" items={of} showOwner {...act} />}
          <Group title="我的" empty="你还没有保存过配置。" items={my} showOwner={false} {...act} />
          <Group title="分享给我的" empty="没有人单独分享配置给你。" items={sh} showOwner {...act} />
          <Group title="公开" empty="没有其他公开的配置。" items={pu} showOwner {...act} />
        </>
      )}
      <ConfirmDialog
        open={!!forget}
        onOpenChange={(o) => !o && setForget(null)}
        title="删除这个配置？"
        confirmLabel="删除配置"
        danger
        onConfirm={() => {
          if (forget) del.mutate(forget.name);
          setForget(null);
        }}
      >
        <span className="mono text-ink">{forget?.name}</span> 会被删除{forget && !forget.mine ? `（${forget.owner} 的配置）` : ""}；用它创建过的 DevPod 不受影响。
      </ConfirmDialog>
    </div>
  );
}
