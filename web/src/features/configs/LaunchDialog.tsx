// 用配置创建 (config center spec §6.3): a field per variable of the
// config, then one DevPod or several. A config pinned to a node takes
// one DevPod per picked node, in click order; any other takes a count.
// The server checks the launch as a whole before writing anything.
import * as Dialog from "@radix-ui/react-dialog";
import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { ApiFailure } from "../../data/api";
import { useMe } from "../../data/queries";
import { Button } from "../../design/Button";
import { Field, Input, Notice } from "../../design/Form";
import { OrderedNodes } from "../devpods/BulkDialog";
import { launchConfig, type ConfigSummary, type LaunchBody, type LaunchItem } from "./api";
import { INDEX, NAME, NODE, OWNER } from "./vars";

export const MAX_LAUNCH = 32;

/** launchNames: the names a launch of n gives, as the server will. With
 *  several, the index is appended — unless the config places ${INDEX}
 *  itself, in which case only the config knows the final names. */
export function launchNames(user: string, short: string, n: number, usesIndex: boolean): string[] {
  const base = `${user}-${short}`;
  return Array.from({ length: n }, (_, i) => (n > 1 && !usesIndex ? `${base}-${i + 1}` : base));
}

type Outcome = { kind: "preview" | "created" | "rejected"; items: LaunchItem[] };

function Items({ outcome }: { outcome: Outcome }) {
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
              <pre className="mono mt-1 max-h-48 overflow-auto rounded border border-line bg-surface p-2 text-[11px] whitespace-pre">{it.yaml}</pre>
            </details>
          )}
        </li>
      ))}
    </ol>
  );
}

export function LaunchDialog({ config, onClose }: { config: Pick<ConfigSummary, "name" | "title" | "variables">; onClose: () => void }) {
  const me = useMe().data;
  const qc = useQueryClient();
  const has = (n: string) => config.variables.some((v) => v.name === n);
  const usesName = has(NAME);
  const usesNode = has(NODE);
  const usesIndex = has(INDEX);
  const others = config.variables.filter((v) => ![OWNER, NAME, NODE, INDEX].includes(v.name));

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
      setOutcome({ kind: r.status === 422 ? "rejected" : dryRun ? "preview" : "created", items: r.items });
      if (!dryRun && r.status !== 422) {
        void qc.invalidateQueries({ queryKey: ["devpods"] });
        void qc.invalidateQueries({ queryKey: ["me"] });
      }
    } catch (e) {
      if (e instanceof ApiFailure && e.status === 404) setGone(true);
      else setError(e instanceof ApiFailure ? e.body.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const made = outcome?.kind === "created" ? outcome.items.filter((i) => i.ok).length : 0;
  const failed = outcome?.kind === "created" ? outcome.items.length - made : 0;
  const done = outcome?.kind === "created";

  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-[12vh] z-50 max-h-[80vh] w-[min(94vw,520px)] -translate-x-1/2 space-y-3 overflow-y-auto rounded-lg border border-line bg-surface p-4 shadow-xl"
        >
          <Dialog.Title className="text-sm font-semibold">
            用配置创建：{config.title} <span className="mono text-xs font-normal text-muted">{config.name}</span>
          </Dialog.Title>
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
              <Field key={v.name} label={v.name} hint={v.required ? "必填" : v.default ? `默认 ${v.default}` : "可留空"}>
                <Input aria-label={v.name} value={values[v.name] ?? ""} onChange={(e) => setValues((m) => ({ ...m, [v.name]: e.target.value }))} className="mono" spellCheck={false} />
              </Field>
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
          {outcome && <Items outcome={outcome} />}
          <div className="flex justify-end gap-2">
            <Dialog.Close asChild>
              <Button>{done ? "关闭" : "取消"}</Button>
            </Dialog.Close>
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
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
