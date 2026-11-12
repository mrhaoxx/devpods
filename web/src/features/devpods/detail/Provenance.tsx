// Where a DevPod came from (config-centric create spec §5.3): its config
// and group under the title; when the config has changed since, a note
// and a rebuild from it — the difference shown first.
import * as Dialog from "@radix-ui/react-dialog";
import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiFailure, getDevPodYaml, rebuildFromConfig, refOf, type DevPod } from "../../../data/api";
import { keyOf, type Ref } from "../../../data/cluster";
import { useDevPods, useMe } from "../../../data/queries";
import { Button } from "../../../design/Button";
import { Notice } from "../../../design/Form";
import { YamlDiff } from "../../../design/yaml/YamlDiff";
import { runBulk } from "../../../lib/bulk";
import { getConfig } from "../../configs/api";
import { provenance } from "../provenance";

const said = (e: unknown) => (e instanceof ApiFailure ? e.body.message : String(e));

function RebuildDialog({ dp, onClose }: { dp: DevPod; onClose: () => void }) {
  const name = dp.metadata.name;
  const ref = refOf(dp);
  const p = provenance(dp);
  const me = useMe().data;
  const qc = useQueryClient();
  const list = useDevPods(me?.user === dp.spec.owner ? "mine" : "all", !!p.group);
  // a group lives in one cluster
  const members: Ref[] = p.group ? (list.data?.items ?? []).filter((x) => x.cluster === dp.cluster && provenance(x).group === p.group).map(refOf) : [ref];
  const [plan, setPlan] = useState<{ now: string; next: string; rebuild: boolean } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [whole, setWhole] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let gone = false;
    Promise.all([getDevPodYaml(ref), rebuildFromConfig(ref, true)])
      .then(([now, next]) => !gone && setPlan({ now: now.yaml, next: next.yaml, rebuild: next.rebuild }))
      .catch((e) => !gone && setError(said(e)));
    return () => {
      gone = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [keyOf(ref)]);

  const run = async () => {
    setBusy(true);
    setError(null);
    const targets = whole && members.length > 1 ? members : [ref];
    const results = await runBulk(targets, (r) => rebuildFromConfig(r));
    setBusy(false);
    void qc.invalidateQueries({ queryKey: ["devpods"] });
    void qc.invalidateQueries({ queryKey: ["devpod"] });
    const failed = results.filter((r) => !r.ok);
    if (failed.length) setError(failed.map((f) => `${f.item.name}：${f.error}`).join("；"));
    else onClose();
  };

  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-[8vh] z-50 max-h-[84vh] w-[min(96vw,980px)] -translate-x-1/2 space-y-3 overflow-y-auto rounded-lg border border-line bg-surface p-4 shadow-xl"
        >
          <Dialog.Title className="text-sm font-semibold">按新配置重建 {name}</Dialog.Title>
          {!plan && !error && <p className="text-xs text-muted">正在按配置渲染…</p>}
          {plan && (
            <>
              <div className="grid grid-cols-2 text-[11px] text-muted">
                <span>现在</span>
                <span>按新配置</span>
              </div>
              <YamlDiff original={plan.now} modified={plan.next} />
              {plan.rebuild && <p className="text-xs text-warm">运行中的 Pod 会重建，已有的会话会断开；home 卷不变。</p>}
              {p.group && members.length > 1 && (
                <fieldset className="flex gap-4 text-xs">
                  <label className="inline-flex items-center gap-1.5">
                    <input type="radio" name="scope" checked={!whole} onChange={() => setWhole(false)} />
                    只重建它
                  </label>
                  <label className="inline-flex items-center gap-1.5">
                    <input type="radio" name="scope" checked={whole} onChange={() => setWhole(true)} />
                    {`整组重建（${members.length} 个）`}
                  </label>
                </fieldset>
              )}
            </>
          )}
          {error && <Notice>{error}</Notice>}
          <div className="flex justify-end gap-2">
            <Dialog.Close asChild>
              <Button>取消</Button>
            </Dialog.Close>
            <Button variant="primary" disabled={!plan || busy} onClick={() => void run()}>
              重建
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

export function Provenance({ dp, outdated }: { dp: DevPod; outdated?: boolean }) {
  const p = provenance(dp);
  const cfg = useQuery({ queryKey: ["config", p.config], queryFn: () => getConfig(p.config!), enabled: !!p.config, retry: false });
  const [ask, setAsk] = useState(false);
  if (!p.config) return null;
  return (
    <div className="space-y-1.5">
      <div className="text-xs text-muted">
        来自配置{" "}
        {cfg.data ? (
          <Link to={`/configs/${p.config}`} className="text-accent hover:underline">
            {cfg.data.title}
          </Link>
        ) : (
          <span className="mono">{p.config}</span>
        )}
        {p.group && (
          <>
            {" · 组 "}
            <Link to={`/?group=${encodeURIComponent(p.group)}`} className="mono text-accent hover:underline">
              {p.group}
            </Link>
            {p.index !== undefined && p.size !== undefined && ` 第 ${p.index}/${p.size} 个`}
          </>
        )}
      </div>
      {outdated && (
        <div role="status" className="flex items-center gap-2 rounded-md bg-warm-soft px-3 py-1.5 text-xs text-warm">
          <span className="flex-1">配置已更新</span>
          <Button onClick={() => setAsk(true)}>按新配置重建</Button>
        </div>
      )}
      {ask && <RebuildDialog dp={dp} onClose={() => setAsk(false)} />}
    </div>
  );
}
