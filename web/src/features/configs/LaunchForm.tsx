// 用配置创建 (config-centric create spec §5.1): a field per variable the
// config's text has — by its type — then one DevPod or several. A config
// pinned to a node takes one DevPod per picked node, in click order; any
// other takes a count. The server checks the launch as a whole before
// writing anything. Shown in the new-DevPod drawer.
import { useEffect, useMemo, useState } from "react";
import { parse } from "yaml";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { ApiFailure } from "../../data/api";
import { useMe } from "../../data/queries";
import { Button } from "../../design/Button";
import { Field, Input, Notice } from "../../design/Form";
import { fmtBytes, fmtCores, parseQuantity } from "../../lib/format";
import { OrderedNodes } from "../devpods/BulkDialog";
import { launchConfig, type ConfigSummary, type LaunchBody, type LaunchItem } from "./api";
import { COUNT, INDEX, LEADER, NAME, NODE, OWNER } from "./vars";
import { VarInput } from "./VarInput";

export const MAX_LAUNCH = 32;

/** launchNames: the names a launch of n gives, as the server will. With
 *  several, the index is appended — unless the config places ${INDEX}
 *  itself, in which case only the config knows the final names. */
export function launchNames(user: string, short: string, n: number, usesIndex: boolean): string[] {
  const base = `${user}-${short}`;
  return Array.from({ length: n }, (_, i) => (n > 1 && !usesIndex ? `${base}-${i}` : base));
}

type Outcome = { kind: "preview" | "created" | "rejected"; items: LaunchItem[]; group?: string };

/** requested: what the DevPods of a checked launch ask for, from their YAML. */
export function requested(yamls: string[]): { cpu: number; memory: number; gpu: number } {
  const out = { cpu: 0, memory: 0, gpu: 0 };
  for (const y of yamls) {
    let doc: { spec?: { pod?: { spec?: { containers?: { resources?: { limits?: Record<string, string | number> } }[] } } } };
    try {
      doc = parse(y) ?? {};
    } catch {
      continue;
    }
    for (const c of doc.spec?.pod?.spec?.containers ?? []) {
      const lim = c.resources?.limits ?? {};
      out.cpu += parseQuantity(lim.cpu === undefined ? undefined : String(lim.cpu));
      out.memory += parseQuantity(lim.memory === undefined ? undefined : String(lim.memory));
      out.gpu += parseQuantity(lim["nvidia.com/gpu"] === undefined ? undefined : String(lim["nvidia.com/gpu"]));
    }
  }
  return out;
}

function Items({ outcome, onEditOnce }: { outcome: Outcome; onEditOnce?: (yaml: string) => void }) {
  return (
    <ol aria-label="结果" className="max-h-56 space-y-1 overflow-y-auto rounded-md border border-line bg-sunk p-2 text-xs">
      {outcome.items.map((it) => (
        <li key={it.index}>
          <div className="flex flex-wrap items-baseline gap-x-2">
            <span aria-hidden="true" className={it.ok ? "text-run" : "text-fail"}>
              {it.ok ? "✓" : "✗"}
            </span>
            {it.ok && outcome.kind === "created" ? (
              <Link to={`/devpods/${it.name}`} className="mono text-accent hover:underline">
                {it.name}
              </Link>
            ) : (
              <span className="mono">{it.name || `#${it.index}`}</span>
            )}
            {it.node && <span className="mono text-muted">→ {it.node}</span>}
            {!it.ok && (
              <span className="text-fail">
                <span className="mono">{it.code}</span>
                {it.message && `：${it.message}`}
              </span>
            )}
          </div>
          {it.yaml && (
            <details className="mt-0.5">
              <summary className="cursor-pointer text-[11px] text-muted">YAML</summary>
              {onEditOnce && outcome.kind === "preview" && (
                <Button className="mt-1" onClick={() => onEditOnce(it.yaml!)} title="把这份 YAML 带进编辑器改一改再创建（不记录来源配置）">
                  只为这次改一改
                </Button>
              )}
              <pre className="mono mt-1 max-h-48 overflow-auto rounded border border-line bg-surface p-2 text-[11px] whitespace-pre">{it.yaml}</pre>
            </details>
          )}
        </li>
      ))}
    </ol>
  );
}

export function LaunchForm({
  config,
  onBack,
  onDone,
  onEditOnce,
}: {
  config: Pick<ConfigSummary, "name" | "title" | "variables" | "owner">;
  onBack?: () => void;
  onDone: (r: { items: LaunchItem[]; group?: string }) => void;
  /** "只为这次改一改": the YAML of one checked instance, to edit and create by hand */
  onEditOnce?: (yaml: string) => void;
}) {
  const me = useMe().data;
  const qc = useQueryClient();
  const has = (n: string) => config.variables.some((v) => v.name === n);
  const usesName = has(NAME);
  const usesNode = has(NODE);
  const usesIndex = has(INDEX);
  const others = config.variables.filter((v) => ![OWNER, NAME, NODE, INDEX, COUNT, LEADER].includes(v.name));

  const [short, setShort] = useState("");
  const [nodes, setNodes] = useState<string[]>([]);
  const [count, setCount] = useState("1");
  const [values, setValues] = useState<Record<string, string>>(() => Object.fromEntries(others.map((v) => [v.name, v.default ?? ""])));
  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [gone, setGone] = useState(false);

  const n = usesNode ? nodes.length : Number(count);
  const countOK = usesNode ? n >= 1 && n <= MAX_LAUNCH : Number.isInteger(n) && n >= 1 && n <= MAX_LAUNCH;
  const missing = others.filter((v) => v.required && values[v.name] === "").map((v) => v.name);
  const ready = !gone && !busy && countOK && (!usesName || short !== "") && missing.length === 0;
  const names = usesName && short && countOK ? launchNames(me?.user ?? "", short, n, usesIndex) : [];

  // a check or a refusal is about what the form said then: once the
  // form changes it is taken down
  useEffect(() => {
    setOutcome((o) => (o && o.kind !== "created" ? null : o));
  }, [short, nodes, count, values]);

  const body = (dryRun: boolean): LaunchBody => ({
    vars: { ...(usesName ? { [NAME]: short } : {}), ...values },
    ...(usesNode ? { nodes } : { count: n }),
    ...(dryRun ? { dryRun: true } : {}),
  });

  const run = async (dryRun: boolean) => {
    setBusy(true);
    setError(null);
    setOutcome(null);
    try {
      const r = await launchConfig(config.name, body(dryRun));
      setOutcome({ kind: r.status === 422 ? "rejected" : dryRun ? "preview" : "created", items: r.items, group: r.group });
      if (!dryRun && r.status !== 422) {
        void qc.invalidateQueries({ queryKey: ["devpods"] });
        void qc.invalidateQueries({ queryKey: ["me"] });
        onDone({ items: r.items, group: r.group });
      }
    } catch (e) {
      if (e instanceof ApiFailure && e.status === 404) setGone(true);
      else setError(e instanceof ApiFailure ? e.body.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const asked = useMemo(() => {
    const ys = (outcome?.kind === "preview" ? outcome.items : []).map((i) => i.yaml ?? "").filter(Boolean);
    return ys.length ? requested(ys) : null;
  }, [outcome]);
  const left = (key: string) => {
    const lim = me?.quota.compute?.[key];
    return lim ? parseQuantity(lim) - parseQuantity(me?.usage.compute?.[key]) : null;
  };
  const made = outcome?.kind === "created" ? outcome.items.filter((i) => i.ok).length : 0;
  const failed = outcome?.kind === "created" ? outcome.items.length - made : 0;
  const done = outcome?.kind === "created";

  return (
    <div className="space-y-3">
      <div className="flex items-baseline justify-between gap-2">
        <div className="min-w-0">
          <div className="truncate text-sm font-semibold">{config.title}</div>
          <div className="mono truncate text-[11px] text-muted">
            {config.name} · {config.owner}
          </div>
        </div>
        <Link to={`/configs/${config.name}`} className="shrink-0 text-xs text-accent hover:underline">
          打开配置
        </Link>
      </div>
          {gone && <Notice>配置已不可用：它被删除，或不再分享给你。</Notice>}
          <fieldset disabled={gone || busy || done} className="space-y-3 text-xs">
            {usesName && (
              <Field label="短名" hint="完整名字会加上你的用户名前缀">
                <div className="flex items-center rounded-md border border-line-strong bg-surface focus-within:border-accent">
                  <span className="mono pl-2.5 text-sm text-faint">{me?.user}-</span>
                  <input aria-label="短名" disabled={gone} value={short} onChange={(e) => setShort(e.target.value.trim())} className="mono h-8 w-full bg-transparent pr-2.5 text-sm focus:outline-none disabled:opacity-60" autoFocus />
                </div>
              </Field>
            )}
            {usesNode ? (
              <OrderedNodes picked={nodes} setPicked={setNodes} legend="节点（每个节点创建一个，按点击顺序）" />
            ) : (
              <Field label="数量" hint={`1 到 ${MAX_LAUNCH}`} error={countOK ? undefined : `数量要在 1 到 ${MAX_LAUNCH} 之间`}>
                <Input aria-label="数量" type="number" min={1} max={MAX_LAUNCH} value={count} onChange={(e) => setCount(e.target.value)} className="mono w-24" />
              </Field>
            )}
            {usesNode && nodes.length > MAX_LAUNCH && (
              <p role="alert" className="text-fail">
                一次最多创建 {MAX_LAUNCH} 个。
              </p>
            )}
            {others.map((v) => (
              <VarInput key={v.name} v={v} value={values[v.name] ?? ""} onChange={(x) => setValues((m) => ({ ...m, [v.name]: x }))} />
            ))}
          </fieldset>
          {names.length > 0 && !outcome && (
            <div className="text-xs">
              <div className="mb-1 font-medium">将创建 {names.length} 个</div>
              <ol aria-label="将创建" className="mono max-h-32 space-y-0.5 overflow-y-auto rounded-md border border-line bg-sunk p-2">
                {names.map((name, i) => (
                  <li key={i}>{usesNode ? `${name} → ${nodes[i]}` : name}</li>
                ))}
              </ol>
              {usesIndex && n > 1 && <p className="mt-1 text-[11px] text-muted">序号由配置里的 {"${INDEX}"} 决定，最终名字见「预览」。</p>}
            </div>
          )}
          {!usesName && !outcome && <p className="text-[11px] text-muted">这个配置的名字是写定的，不能在这里改。</p>}
          {error && <Notice>{error}</Notice>}
          {outcome?.kind === "rejected" && <Notice>没有创建任何 DevPod：下面标出的几个无法创建，改好后再试。</Notice>}
          {outcome?.kind === "preview" && (
            <p role="status" className="rounded-md bg-run-soft px-3 py-2 text-xs text-run">
              检查通过，尚未创建。展开可以看每个 DevPod 的 YAML。
            </p>
          )}
          {done && (
            <p role="status" className={`rounded-md px-3 py-2 text-xs ${failed ? "bg-warm-soft text-warm" : "bg-run-soft text-run"}`}>
              {failed ? `已创建 ${made} 个，失败 ${failed} 个。已创建的不会撤销。` : `已创建 ${made} 个。`}
            </p>
          )}
          {asked && (
            <p aria-label="本次申请" className="rounded-md bg-sunk px-3 py-2 text-xs">
              本次申请 {fmtCores(asked.cpu)} 核 CPU · {fmtBytes(asked.memory)} 内存 · {asked.gpu} GPU
              {[left("cpu"), left("memory"), left("nvidia.com/gpu")].some((x) => x !== null) && (
                <span className="text-muted">
                  {" "}
                  （剩余配额：{left("cpu") !== null ? `${fmtCores(Math.max(0, left("cpu")!))} 核` : "不限"} · {left("memory") !== null ? fmtBytes(Math.max(0, left("memory")!)) : "不限"} ·{" "}
                  {left("nvidia.com/gpu") !== null ? `${Math.max(0, left("nvidia.com/gpu")!)} GPU` : "不限"}）
                </span>
              )}
            </p>
          )}
          {outcome && <Items outcome={outcome} onEditOnce={onEditOnce} />}
          <div className="flex justify-end gap-2">
            {onBack && !done && <Button onClick={onBack}>上一步</Button>}
            {!done && (
              <>
                <Button disabled={!ready} onClick={() => void run(true)}>
                  预览
                </Button>
                <Button variant="primary" disabled={!ready} onClick={() => void run(false)}>
                  创建
                </Button>
              </>
            )}
          </div>
    </div>
  );
}
