// Batch actions on selected users (UI spec §5.5): 设配额, 设审计策略,
// 重置密码 (passwords shown once, downloadable as CSV), 休眠全部 DevPod.
import * as Dialog from "@radix-ui/react-dialog";
import { useState } from "react";
import { usersBatch, ApiFailure, type BatchOp, type BatchResult, type QuotaForm } from "../../data/api";
import { formFromPolicy, policyFromForm, type PolicyForm } from "../../lib/policy";
import { Button } from "../../design/Button";
import { Field, Input, Notice } from "../../design/Form";
import { PolicyFields } from "../audit/AuditPolicyEditor";
import { csvDownload, saveBlob } from "./csv";

const titles: Record<BatchOp, string> = {
  setQuota: "设配额",
  setPolicy: "设审计策略",
  resetPassword: "重置密码",
  hibernateAll: "休眠其全部 DevPod",
};
const confirmLabel: Record<BatchOp, string> = { setQuota: "应用配额", setPolicy: "应用策略", resetPassword: "确认重置", hibernateAll: "确认休眠" };

export function BatchDialog({ op, names, onClose }: { op: BatchOp; names: string[]; onClose: (changed: boolean) => void }) {
  const [quota, setQuota] = useState<{ maxDevPods: string; cpu: string; memory: string; gpu: string; storage: string }>({ maxDevPods: "", cpu: "", memory: "", gpu: "", storage: "" });
  const [policy, setPolicy] = useState<PolicyForm>(() => formFromPolicy(null));
  const [inherit, setInherit] = useState(false);
  const [results, setResults] = useState<BatchResult[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const run = async () => {
    setErr(null);
    setBusy(true);
    try {
      const args =
        op === "setQuota"
          ? {
              quota: {
                maxDevPods: quota.maxDevPods ? Number(quota.maxDevPods) : undefined,
                cpu: quota.cpu || undefined,
                memory: quota.memory || undefined,
                gpu: quota.gpu || undefined,
                storage: quota.storage || undefined,
              } as QuotaForm,
            }
          : op === "setPolicy"
            ? { policy: inherit ? null : policyFromForm(policy) }
            : undefined;
      const r = await usersBatch({ names, op, args });
      setResults(r.results);
    } catch (e) {
      setErr(e instanceof ApiFailure ? e.body.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const failed = results?.filter((r) => !r.ok) ?? [];
  const passwords = results?.filter((r) => r.password) ?? [];

  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose(!!results)}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-[12%] z-50 max-h-[80vh] w-[min(94vw,560px)] -translate-x-1/2 space-y-3 overflow-y-auto rounded-lg border border-line bg-surface p-4 shadow-xl"
        >
          <Dialog.Title className="text-sm font-semibold">
            {titles[op]}（{names.length} 个用户）
          </Dialog.Title>
          {!results && (
            <>
              {op === "setQuota" && (
                <div className="grid grid-cols-3 gap-2">
                  {(["maxDevPods", "cpu", "memory", "gpu", "storage"] as const).map((k) => (
                    <Field key={k} label={{ maxDevPods: "DevPod 数", cpu: "CPU", memory: "内存", gpu: "GPU", storage: "存储" }[k]}>
                      <Input aria-label={k} value={quota[k]} onChange={(e) => setQuota({ ...quota, [k]: e.target.value })} placeholder="默认" className="mono" />
                    </Field>
                  ))}
                  <p className="col-span-3 text-[11px] text-faint">全部留空 = 恢复为全局默认配额。</p>
                </div>
              )}
              {op === "setPolicy" && (
                <div className="space-y-2">
                  <label className="inline-flex items-center gap-1.5 text-xs">
                    <input type="checkbox" checked={inherit} onChange={(e) => setInherit(e.target.checked)} /> 恢复为集群默认策略
                  </label>
                  {!inherit && <PolicyFields form={policy} onChange={setPolicy} />}
                </div>
              )}
              {op === "resetPassword" && <p className="text-xs text-muted">给每个人生成一个随机密码，原密码立即失效。新密码只显示这一次。</p>}
              {op === "hibernateAll" && <p className="text-xs text-muted">休眠这些用户名下所有运行中的 DevPod（数据保留；没有持久化的容器内数据会丢失）。</p>}
              {err && <Notice>{err}</Notice>}
              <div className="flex justify-end gap-2">
                <Button onClick={() => onClose(false)}>取消</Button>
                <Button variant={op === "setQuota" || op === "setPolicy" ? "primary" : "danger"} disabled={busy} onClick={() => void run()}>
                  {confirmLabel[op]}
                </Button>
              </div>
            </>
          )}
          {results && (
            <>
              <Notice tone={failed.length ? "warm" : "run"}>
                成功 {results.length - failed.length}，失败 {failed.length}
              </Notice>
              {passwords.length > 0 && (
                <div className="space-y-2">
                  <p className="text-xs text-warm">新密码只显示这一次，请现在保存。</p>
                  <table className="w-full text-xs">
                    <tbody>
                      {passwords.map((r) => (
                        <tr key={r.name} className="border-b border-line">
                          <td className="mono py-1">{r.name}</td>
                          <td className="mono py-1 select-all">{r.password}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                  <Button onClick={() => saveBlob(csvDownload([["username", "password"], ...passwords.map((r) => [r.name, r.password!])]), "passwords.csv")}>
                    下载 CSV
                  </Button>
                </div>
              )}
              {failed.length > 0 && (
                <ul className="text-xs">
                  {failed.map((r) => (
                    <li key={r.name} className="text-fail">
                      <span className="mono">{r.name}</span>：{r.error}
                    </li>
                  ))}
                </ul>
              )}
              {op === "hibernateAll" && (
                <p className="text-xs text-muted">共休眠 {results.reduce((a, r) => a + (r.hibernated ?? 0), 0)} 个 DevPod。</p>
              )}
              <div className="flex justify-end">
                <Button variant="primary" onClick={() => onClose(true)}>
                  完成
                </Button>
              </div>
            </>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
