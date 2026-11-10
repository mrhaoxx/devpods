// CSV import (UI spec §5.5): username,displayName[,password]; preview
// (dryRun) first, then commit; generated passwords are shown once.
import * as Dialog from "@radix-ui/react-dialog";
import { useState } from "react";
import { importUsers, ApiFailure, type ImportResult } from "../../data/api";
import { Button } from "../../design/Button";
import { Notice, Textarea } from "../../design/Form";
import { csvDownload, saveBlob } from "./csv";

function Rows({ rows }: { rows: ImportResult[] }) {
  return (
    <table className="w-full text-xs">
      <thead>
        <tr className="border-b border-line text-left text-[11px] text-muted">
          <th className="py-1">行</th>
          <th>用户名</th>
          <th>显示名</th>
          <th>结果</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => (
          <tr key={r.line + r.username} className="border-b border-line">
            <td className="mono py-1">{r.line}</td>
            <td className="mono">{r.username}</td>
            <td>{r.displayName}</td>
            <td className={r.ok ? "text-run" : "text-fail"}>{r.ok ? (r.password ? <span className="mono select-all">{r.password}</span> : "可导入") : r.error}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function ImportDialog({ onClose }: { onClose: (changed: boolean) => void }) {
  const [csv, setCsv] = useState("");
  const [preview, setPreview] = useState<ImportResult[] | null>(null);
  const [done, setDone] = useState<{ rows: ImportResult[]; created: number } | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const call = async (dry: boolean) => {
    setErr(null);
    setBusy(true);
    try {
      const r = await importUsers(csv, dry);
      if (dry) setPreview(r.rows);
      else setDone(r);
    } catch (e) {
      setErr(e instanceof ApiFailure ? e.body.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const valid = preview?.filter((r) => r.ok).length ?? 0;
  const generated = done?.rows.filter((r) => r.password) ?? [];

  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose(!!done)}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-[10%] z-50 max-h-[84vh] w-[min(94vw,640px)] -translate-x-1/2 space-y-3 overflow-y-auto rounded-lg border border-line bg-surface p-4 shadow-xl"
        >
          <Dialog.Title className="text-sm font-semibold">导入 CSV</Dialog.Title>
          {!done ? (
            <>
              <p className="text-xs text-muted">
                每行 <code className="mono">username,displayName[,password]</code>；不填密码就随机生成。可以有表头行。
              </p>
              <input
                type="file"
                accept=".csv,text/csv"
                aria-label="选择 CSV 文件"
                className="text-xs"
                onChange={async (e) => {
                  const f = e.target.files?.[0];
                  if (f) {
                    setCsv(await f.text());
                    setPreview(null);
                  }
                }}
              />
              <Textarea
                aria-label="CSV 内容"
                rows={6}
                value={csv}
                onChange={(e) => {
                  setCsv(e.target.value);
                  setPreview(null);
                }}
                className="mono text-xs"
                spellCheck={false}
              />
              {err && <Notice>{err}</Notice>}
              {preview && <Rows rows={preview} />}
              <div className="flex items-center justify-end gap-2">
                {preview && <span className="text-xs text-muted">可导入 {valid} / {preview.length} 行</span>}
                <Button onClick={() => onClose(false)}>取消</Button>
                <Button disabled={!csv.trim() || busy} onClick={() => void call(true)}>
                  预览
                </Button>
                <Button variant="primary" disabled={!preview || valid === 0 || busy} onClick={() => void call(false)}>
                  确认导入
                </Button>
              </div>
            </>
          ) : (
            <>
              <Notice tone="run">已创建 {done.created} 个用户</Notice>
              {generated.length > 0 && <p className="text-xs text-warm">生成的密码只显示这一次，请现在保存。</p>}
              <Rows rows={done.rows} />
              <div className="flex justify-end gap-2">
                {generated.length > 0 && (
                  <Button onClick={() => saveBlob(csvDownload([["username", "password"], ...generated.map((r) => [r.username, r.password!])]), "imported-users.csv")}>
                    下载 CSV
                  </Button>
                )}
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
