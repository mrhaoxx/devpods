// 保存为镜像 (snapshot spec v2): a form, then the same dialog shows the
// snapshot happening — the three steps, elapsed time, the Job's log tail
// — with a cancel button. Closing the dialog cancels nothing.
import * as Dialog from "@radix-ui/react-dialog";
import { useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { ApiFailure, createSnapshot, deleteSnapshot, streamSnapshotProgress, type SnapshotProgress, type SnapshotRequest, type SnapshotView } from "../../data/api";
import { lastImage, lastTagMode, rememberImage, rememberTagMode, timestampTag, withTag, type TagMode } from "./lastImage";
import { useMe } from "../../data/queries";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { CopyField } from "../../design/Copy";
import { Field, Input, Notice } from "../../design/Form";
import { fmtBytes } from "../../lib/format";
import { savedRegistryFor } from "../../lib/registry";
import { useRegistries } from "../settings/Registries";
import { defaultSnapshotName, fmtElapsed, NAME_RE, STEPS, stepStates, validImageRef, type StepState } from "./progress";

const mark: Record<StepState, string> = { todo: "○", active: "◐", done: "●", failed: "✕" };
const tone: Record<StepState, string> = { todo: "text-faint", active: "text-accent", done: "text-run", failed: "text-fail" };
const word: Record<StepState, string> = { todo: "未开始", active: "进行中", done: "已完成", failed: "失败" };

/** SnapshotProgressView follows one snapshot until it ends. */
export function SnapshotProgressView({ snapshot, onClose }: { snapshot: Pick<SnapshotView, "id" | "image">; onClose: () => void }) {
  const qc = useQueryClient();
  const [p, setP] = useState<SnapshotProgress | null>(null);
  const [broken, setBroken] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const lastStage = useRef<SnapshotProgress["stage"]>("commit");
  const log = useRef<HTMLPreElement>(null);

  useEffect(() => {
    setP(null);
    setBroken(false);
    return streamSnapshotProgress(
      snapshot.id,
      (ev) => {
        if (ev.stage === "commit" || ev.stage === "push") lastStage.current = ev.stage;
        setP(ev);
      },
      (clean) => {
        if (!clean) setBroken(true);
        void qc.invalidateQueries({ queryKey: ["snapshots"] });
      },
    );
  }, [snapshot.id, qc]);
  useEffect(() => {
    if (log.current) log.current.scrollTop = log.current.scrollHeight;
  }, [p?.lines]);

  const cancel = useMutation({ mutationFn: () => deleteSnapshot(snapshot.id), onSettled: () => void qc.invalidateQueries({ queryKey: ["snapshots"] }) });
  const stage = p?.stage ?? "pending";
  const running = stage !== "done" && stage !== "error";
  const states = stepStates(stage, lastStage.current);

  return (
    <div className="space-y-3 text-xs">
      <ol className="flex flex-wrap items-center gap-x-4 gap-y-1" aria-label="进度">
        {STEPS.map((s, i) => (
          <li key={s.id} className="flex items-center gap-1.5" aria-current={states[i] === "active" ? "step" : undefined}>
            <span className={tone[states[i]]} aria-hidden>
              {mark[states[i]]}
            </span>
            <span className={states[i] === "todo" ? "text-muted" : "font-medium text-ink"}>{s.label}</span>
            <span className="sr-only">（{word[states[i]]}）</span>
          </li>
        ))}
      </ol>
      <div className="mono flex flex-wrap gap-x-4 gap-y-0.5 text-muted" role="status">
        <span>
          {stage === "pending" ? "等待开始" : stage === "commit" ? "正在提交容器" : stage === "push" ? "正在推送" : stage === "done" ? "已完成" : "未完成"} · 用时{" "}
          <span className="text-ink">{fmtElapsed(p?.elapsedSeconds ?? 0)}</span>
        </span>
        {!!p?.sizeBytes && (
          <span>
            镜像大小 <span className="text-ink">{fmtBytes(p.sizeBytes)}</span>
          </span>
        )}
        {stage === "push" && !!p?.totalBytes && (
          <span>
            已推送{" "}
            <span className="text-ink">
              {fmtBytes(p.pushedBytes ?? 0)} / {fmtBytes(p.totalBytes)}
            </span>
          </span>
        )}
      </div>
      {stage === "push" && !!p?.totalBytes && (
        <div
          role="progressbar"
          aria-label="推送进度"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(((p.pushedBytes ?? 0) / p.totalBytes) * 100)}
          className="h-1.5 overflow-hidden rounded-full bg-line"
        >
          <div className="h-full rounded-full bg-accent transition-[width] duration-500" style={{ width: `${Math.min(100, ((p.pushedBytes ?? 0) / p.totalBytes) * 100)}%` }} />
        </div>
      )}
      <pre ref={log} aria-label="输出" className="mono h-40 overflow-auto rounded-md border border-line bg-sunk p-2 text-[11px] leading-relaxed whitespace-pre-wrap">
        {p?.lines.length ? p.lines.join("\n") : running ? "等待快照任务的输出…" : "（没有输出）"}
      </pre>
      {stage === "done" && (
        <div className="space-y-2">
          <Notice tone="run">镜像已保存。</Notice>
          <CopyField value={snapshot.image} label="复制镜像地址" />
          {p?.digest && <div className="mono truncate text-[11px] text-faint">{p.digest}</div>}
        </div>
      )}
      {stage === "error" && <Notice>{p?.message || "保存失败"}</Notice>}
      {broken && running && <Notice tone="warm">进度连接中断了；保存仍在后台进行，可以在「我的镜像」里查看结果。</Notice>}
      <div className="flex justify-end gap-2">
        {running && (
          <Button variant="danger" onClick={() => setConfirm(true)} disabled={cancel.isPending}>
            取消保存
          </Button>
        )}
        {stage === "done" && (
          <Link to={`/?new=1&image=${encodeURIComponent(snapshot.image)}`} className="inline-flex h-7 items-center rounded-md border border-line px-2.5 text-xs hover:bg-sunk" onClick={onClose}>
            用它新建 DevPod
          </Link>
        )}
        <Button onClick={onClose}>{running ? "关闭（继续在后台保存）" : "关闭"}</Button>
      </div>
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title="取消这次保存？"
        confirmLabel="取消保存"
        danger
        onConfirm={() => {
          setConfirm(false);
          cancel.mutate();
        }}
      >
        快照任务会被终止，已经提交或推送的部分不保留。
      </ConfirmDialog>
    </div>
  );
}

function CreateForm({ devpod, owner, onCreated }: { devpod: string; owner: string; onCreated: (s: SnapshotView) => void }) {
  const me = useMe().data;
  const hasDefault = !!me?.features.snapshotDefaultRegistry;
  // the address used last time (this DevPod's, else the user's last one)
  // comes back prefilled; a tag that was already saved must be changed,
  // and the server says so
  const user = me?.user ?? "";
  const touched = useRef(false); // the user picked a target or typed an address
  const [target, setTargetState] = useState<"default" | "custom">(hasDefault ? "default" : "custom");
  const [name, setName] = useState(defaultSnapshotName(devpod, owner));
  const [tag, setTag] = useState("latest");
  const [image, setImageState] = useState("");
  // keep: the tag given is used, and saving to the same address again
  // overwrites it; new: the tag is the date and time, made here in the
  // browser when the save starts
  const [tagMode, setTagModeState] = useState<TagMode>("keep");
  const setTagMode = (m: TagMode) => {
    setTagModeState(m);
    rememberTagMode(user, m);
  };
  const setTarget = (t: "default" | "custom") => {
    touched.current = true;
    setTargetState(t);
  };
  const setImage = (v: string) => {
    touched.current = true;
    setImageState(v);
  };
  useEffect(() => {
    setTagModeState(lastTagMode(user));
    const last = lastImage(user, devpod);
    if (last && !touched.current) {
      setImageState(last);
      setTargetState("custom");
    }
  }, [user, devpod]);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [pause, setPause] = useState(true);
  // a registry the user saved a login for (设置 → 镜像仓库凭证) needs no
  // credentials here: the server pushes with the saved one
  const registries = useRegistries().data?.items ?? [];
  const [useOther, setUseOther] = useState(false);
  const saved = useOther ? undefined : savedRegistryFor(image, registries);
  const create = useMutation({
    mutationFn: (body: SnapshotRequest) => createSnapshot(devpod, body),
    onSuccess: (s) => {
      setPassword(""); // used once; not kept in the page
      if (target === "custom") rememberImage(user, devpod, image);
      onCreated(s);
    },
  });
  const fresh = tagMode === "new";
  // with a generated tag the address may be typed without one
  const imageOK = validImageRef(fresh ? withTag(image, "t") : image);
  const ok = target === "default" ? NAME_RE.test(name) && (fresh || NAME_RE.test(tag)) : imageOK && (!!saved || (useOther ? username !== "" && password !== "" : (username === "") === (password === "")));
  const submit = () => {
    if (!ok) return;
    const stamp = timestampTag(new Date());
    const ref = fresh ? withTag(image, stamp) : image;
    create.mutate(target === "default" ? { name, tag: fresh ? stamp : tag, pause } : saved || username === "" ? { image: ref, pause } : { image: ref, pushAuth: { username, password }, pause });
  };
  const err = create.error;
  return (
    <form
      className="space-y-3 text-xs"
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      {hasDefault && (
        <div role="radiogroup" aria-label="目标" className="inline-flex rounded-md border border-line bg-sunk p-0.5">
          {(
            [
              ["default", "默认仓库"],
              ["custom", "自定义地址"],
            ] as const
          ).map(([id, label]) => (
            <button key={id} type="button" role="radio" aria-checked={target === id} onClick={() => setTarget(id)} className={`rounded px-2.5 py-0.5 ${target === id ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}>
              {label}
            </button>
          ))}
        </div>
      )}
      {target === "default" ? (
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="名称" error={name && !NAME_RE.test(name) ? "小写字母、数字和 . _ -，以字母或数字开头" : undefined}>
            <Input aria-label="名称" className="mono" value={name} onChange={(e) => setName(e.target.value)} required autoFocus />
          </Field>
          {!fresh && (
            <Field label="标签" error={!NAME_RE.test(tag) ? "小写字母、数字和 . _ -，不能为空" : undefined}>
              <Input aria-label="标签" className="mono" value={tag} onChange={(e) => setTag(e.target.value)} required />
            </Field>
          )}
        </div>
      ) : (
        <div className="space-y-3">
          <Field
            label="镜像地址"
            hint={fresh ? "完整地址；标签会被换成日期时间" : "完整地址，带标签"}
            error={image && !imageOK ? (fresh ? "形如 registry.example.com/team/name" : "形如 registry.example.com/team/name:tag") : undefined}
          >
            <Input aria-label="镜像地址" className="mono" value={image} onChange={(e) => setImage(e.target.value)} placeholder="registry.example.com/team/name:tag" required autoFocus />
          </Field>
          {saved ? (
            <p className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md bg-sunk px-3 py-2" role="status">
              <span>
                将使用已保存的凭证（用户名 <span className="mono text-ink">{saved.username}</span>）
                {saved.pending && <span className="text-warm">；它还在写入集群，稍等片刻再保存更稳妥</span>}
              </span>
              <button type="button" onClick={() => setUseOther(true)} className="text-accent hover:underline">
                改用其他凭证
              </button>
            </p>
          ) : (
            <>
              <div className="grid gap-3 sm:grid-cols-2">
                <Field label="用户名">
                  <Input aria-label="用户名" className="mono" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
                </Field>
                <Field label="密码">
                  <Input aria-label="密码" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
                </Field>
              </div>
              <p className="text-[11px] text-muted">
                仓库不需要登录就留空。用户名和密码只用于这一次推送，控制台不保存，之后也不会再显示。常用的仓库可以在 设置 → 镜像仓库凭证 里保存一次。
                {useOther && (
                  <button type="button" onClick={() => setUseOther(false)} className="ml-1 text-accent hover:underline">
                    改回已保存的凭证
                  </button>
                )}
              </p>
            </>
          )}
        </div>
      )}
      <div className="space-y-1">
        <div role="radiogroup" aria-label="标签方式" className="inline-flex rounded-md border border-line bg-sunk p-0.5">
          {(
            [
              ["keep", "保留标签"],
              ["new", "生成新标签"],
            ] as const
          ).map(([id, label]) => (
            <button key={id} type="button" role="radio" aria-checked={tagMode === id} onClick={() => setTagMode(id)} className={`rounded px-2.5 py-0.5 ${tagMode === id ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}>
              {label}
            </button>
          ))}
        </div>
        <p className="text-[11px] text-muted">
          {fresh ? (
            <>
              开始保存时用当前日期时间作标签，例如 <span className="mono">{timestampTag(new Date())}</span>；每次保存都是一个新镜像。
            </>
          ) : (
            "用上面填的标签；这个地址之前保存过的话会被覆盖，旧的那条记录随之移除。"
          )}
        </p>
      </div>
      <label className="flex items-start gap-1.5">
        <input type="checkbox" className="mt-0.5" checked={pause} onChange={(e) => setPause(e.target.checked)} />
        <span>
          提交时暂停容器（推荐）
          <span className="block text-[11px] text-muted">暂停期间容器里的程序和 SSH 会卡住几秒到几十秒，换来一份内容一致的镜像。不暂停时，正在写入的文件可能只保存了一半。</span>
        </span>
      </label>
      <Notice tone="warm">
        <ul className="list-disc space-y-0.5 pl-4">
          <li>镜像只包含容器自身的文件系统，不含持久卷（通常是 home 目录）。</li>
          <li>每次保存都在原镜像上再加一层，反复保存会越来越大。</li>
        </ul>
      </Notice>
      {err != null && <Notice>{err instanceof ApiFailure ? err.body.message : String(err)}</Notice>}
      <div className="flex justify-end gap-2">
        <Dialog.Close asChild>
          <Button>取消</Button>
        </Dialog.Close>
        <Button type="submit" variant="primary" disabled={!ok || create.isPending}>
          开始保存
        </Button>
      </div>
    </form>
  );
}

/** SnapshotDialog: `following` set = show that snapshot's progress;
 *  otherwise the form, which turns into the progress view on submit. */
export function SnapshotDialog({ devpod, owner, following, onClose }: { devpod: string; owner: string; following?: Pick<SnapshotView, "id" | "image">; onClose: () => void }) {
  const qc = useQueryClient();
  const [current, setCurrent] = useState<Pick<SnapshotView, "id" | "image"> | undefined>(following);
  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content aria-describedby={undefined} className="fixed left-1/2 top-[12vh] z-50 max-h-[80vh] w-[min(94vw,560px)] -translate-x-1/2 space-y-3 overflow-y-auto rounded-lg border border-line bg-surface p-4 shadow-xl">
          <Dialog.Title className="text-sm font-semibold">
            保存为镜像 · <span className="mono">{devpod}</span>
          </Dialog.Title>
          {current ? (
            <>
              <div className="mono truncate text-[11px] text-muted" title={current.image}>
                {current.image}
              </div>
              <SnapshotProgressView snapshot={current} onClose={onClose} />
            </>
          ) : (
            <CreateForm
              devpod={devpod}
              owner={owner}
              onCreated={(s) => {
                void qc.invalidateQueries({ queryKey: ["snapshots"] });
                setCurrent(s);
              }}
            />
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
