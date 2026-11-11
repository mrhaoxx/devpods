// 新建 / 编辑 DevPod (UI spec §5.6). The node picker arrives with the
// reservation backend (plan ruling R1).
import { NodePicker } from "./NodePicker";
import { fmtTime } from "../../lib/reservations";
import { useMemo, useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createDevPod, patchDevPodSpec, ApiFailure, type DevPod, type SpecPatch } from "../../data/api";
import { useMe, useTemplates } from "../../data/queries";
import { Drawer } from "../../design/Drawer";
import { Button } from "../../design/Button";
import { Field, Input, Notice, Select, Textarea } from "../../design/Form";
import { parseQuantity } from "../../lib/format";
import { quotaErrors, serverQuotaErrors, type QuotaField } from "./quota";
import { MY_IMAGES_LIST, MyImagesList } from "../images/MyImages";
import { getConfig, listConfigs } from "../configs/api";

type Props = { mode: "create"; devpod?: undefined; initialImage?: string; open: boolean; onClose(): void } | { mode: "edit"; devpod: DevPod; open: boolean; onClose(): void };

const target = (dp?: DevPod) => dp?.spec.pod?.spec?.containers?.[0];

export function DevPodDrawer(p: Props) {
  const me = useMe().data;
  const tpl = useTemplates();
  const nav = useNavigate();
  const qc = useQueryClient();
  const editing = p.mode === "edit";
  const c = target(p.devpod);
  const lim = c?.resources?.limits ?? {};

  const [template, setTemplate] = useState("");
  const [overlay, setOverlay] = useState("");
  const [yaml, setYaml] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [image, setImage] = useState(c?.image ?? (p.mode === "create" ? p.initialImage : undefined) ?? "ubuntu:24.04");
  const [cpu, setCpu] = useState(lim.cpu ?? "2");
  const [memory, setMemory] = useState(lim.memory ?? "4Gi");
  const [gpu, setGpu] = useState(lim["nvidia.com/gpu"] ?? "");
  const [shell, setShell] = useState(p.devpod?.spec.shell ?? "");
  const [persist, setPersist] = useState("");
  const [collab, setCollab] = useState((p.devpod?.spec.collaborators ?? []).join(", "));
  const pinned = p.devpod?.spec.pod?.spec?.nodeSelector?.["kubernetes.io/hostname"] ?? "";
  const [node, setNode] = useState(pinned);
  const [serverErr, setServerErr] = useState<{ fields: Partial<Record<QuotaField, string>>; text?: string } | null>(null);
  const [busy, setBusy] = useState(false);

  // saved configs to load into the YAML editor (asked for only there)
  const configs = useQuery({ queryKey: ["configs", "all"], queryFn: () => listConfigs(), enabled: !editing && yaml !== null, retry: false });
  const loadConfig = (name: string) =>
    getConfig(name)
      .then((c) => setYaml(c.content))
      .catch((e) => fail(e));

  const templates = tpl.data?.items ?? [];
  const presets = templates.filter((t) => t.spec.podPreset);
  const overlays = templates.filter((t) => t.spec.binding && !t.spec.podPreset);
  const preset = !!template;

  const own = editing
    ? { cpu: parseQuantity(lim.cpu), memory: parseQuantity(lim.memory), gpu: parseQuantity(lim["nvidia.com/gpu"]), running: !!p.devpod?.spec.running }
    : undefined;
  const live = useMemo(
    () => (me && !preset && yaml === null ? quotaErrors({ cpu, memory, gpu, storage: persist }, me.quota, me.usage, own) : {}),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [me, preset, yaml, cpu, memory, gpu, persist, own?.cpu, own?.memory, own?.gpu, own?.running],
  );
  const errs = { ...live, ...(serverErr?.fields ?? {}) };
  const collaborators = collab
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);

  const rebuild =
    editing &&
    !!p.devpod?.spec.running &&
    (image !== (c?.image ?? "") ||
      cpu !== (lim.cpu ?? "") ||
      memory !== (lim.memory ?? "") ||
      gpu !== (lim["nvidia.com/gpu"] ?? "") ||
      shell !== (p.devpod?.spec.shell ?? "") ||
      node !== pinned);

  const fail = (e: unknown) => {
    if (e instanceof ApiFailure && e.code === "NODE_RESERVED") {
      const d = e.body.detail as { node?: string; user?: string; until?: string } | undefined;
      setServerErr({ fields: {}, text: d?.until ? `节点 ${d.node} 正被 ${d.user} 预约至 ${fmtTime(d.until)}，现在不能使用。` : e.body.message });
    } else if (e instanceof ApiFailure && e.code === "QUOTA_EXCEEDED") setServerErr({ fields: serverQuotaErrors(e.body.detail), text: e.body.message });
    else setServerErr({ fields: {}, text: e instanceof ApiFailure ? e.body.message : String(e) });
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setServerErr(null);
    setBusy(true);
    try {
      if (editing) {
        const body: SpecPatch = {};
        if (image !== c?.image) body.image = image;
        if (cpu !== (lim.cpu ?? "")) body.cpu = cpu;
        if (memory !== (lim.memory ?? "")) body.memory = memory;
        if (gpu !== (lim["nvidia.com/gpu"] ?? "")) body.gpu = gpu;
        if (shell !== (p.devpod.spec.shell ?? "")) body.shell = shell;
        if (collaborators.join(",") !== (p.devpod.spec.collaborators ?? []).join(",")) body.collaborators = collaborators;
        if (node !== pinned) body.node = node;
        await patchDevPodSpec(p.devpod.metadata.name, body);
        void qc.invalidateQueries({ queryKey: ["devpod", p.devpod.metadata.name] });
      } else {
        let body: Record<string, unknown>;
        if (yaml !== null) body = { yaml, ...(overlay ? { templateRef: overlay } : {}) };
        else if (preset) body = { name, templateRef: template };
        else {
          body = { name, image, cpu, memory };
          if (gpu && gpu !== "0") body.gpu = gpu;
          if (shell) body.shell = shell;
          if (persist) body.persistence = { size: persist, mountPath: "/home/dev" };
          if (overlay) body.templateRef = overlay;
        }
        if (yaml === null && node) body.node = node;
        if (yaml === null && collaborators.length) body.collaborators = collaborators;
        const dp = await createDevPod(body);
        nav(`/devpods/${dp.metadata.name}`);
      }
      void qc.invalidateQueries({ queryKey: ["devpods"] });
      void qc.invalidateQueries({ queryKey: ["me"] });
      p.onClose();
    } catch (err) {
      fail(err);
    } finally {
      setBusy(false);
    }
  };

  const budget = me?.nameBudget ?? 21;
  return (
    <Drawer open={p.open} onOpenChange={(o) => !o && p.onClose()} title={editing ? `编辑 ${p.devpod.metadata.name}` : "新建 DevPod"} width={480}>
      <form onSubmit={submit} className="space-y-3">
        {!editing && (
          <div className="flex items-center gap-2 text-xs">
            <label className="inline-flex items-center gap-1.5">
              <input type="checkbox" checked={yaml !== null} onChange={(e) => setYaml(e.target.checked ? "" : null)} />
              使用 YAML
            </label>
          </div>
        )}
        {yaml !== null ? (
          <>
            <div className="flex flex-wrap items-center gap-2">
              {(configs.data?.items.length ?? 0) > 0 && (
                <Select aria-label="从配置载入" value="" onChange={(e) => e.target.value && void loadConfig(e.target.value)} className="!w-auto min-w-0 flex-1 text-xs">
                  <option value="">从配置载入…</option>
                  {configs.data!.items.map((c) => (
                    <option key={c.name} value={c.name}>
                      {c.title}（{c.name}）
                    </option>
                  ))}
                </Select>
              )}
              <Button disabled={!yaml.trim()} onClick={() => nav("/configs/new", { state: { content: yaml } })} title="把编辑器里的内容存成可复用、可分享的配置">
                保存为配置
              </Button>
            </div>
            <Field label="DevPod YAML" hint="配置里的变量（${NAME} 等）要先替换成实际值；要填变量并批量创建，请到「配置」页">
              <Textarea aria-label="DevPod YAML" rows={14} value={yaml} onChange={(e) => setYaml(e.target.value)} className="mono text-xs" spellCheck={false} />
            </Field>
          </>
        ) : (
          <>
            {!editing && presets.length > 0 && (
              <Field label="模板">
                <Select aria-label="模板" value={template} onChange={(e) => setTemplate(e.target.value)}>
                  <option value="">自定义</option>
                  {presets.map((t) => (
                    <option key={t.metadata.name} value={t.metadata.name}>
                      {t.spec.displayName}
                    </option>
                  ))}
                </Select>
              </Field>
            )}
            {!editing && (
              <Field label="名称" hint={`还可输入 ${budget - name.length} 个字符`} error={errs.count}>
                <div className="flex items-center rounded-md border border-line-strong bg-surface focus-within:border-accent">
                  <span className="mono pl-2.5 text-sm text-faint">{me?.user}-</span>
                  <input
                    aria-label="名称"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    maxLength={budget}
                    required
                    className="mono h-8 w-full bg-transparent pr-2.5 text-sm focus:outline-none"
                  />
                </div>
              </Field>
            )}
            {!preset && (
              <>
                <Field label="镜像">
                  <Input aria-label="镜像" value={image} onChange={(e) => setImage(e.target.value)} className="mono" list={MY_IMAGES_LIST} required />
                  <MyImagesList />
                </Field>
                <div className="grid grid-cols-3 gap-2">
                  <Field label="CPU" error={errs.cpu}>
                    <Input aria-label="CPU" value={cpu} onChange={(e) => setCpu(e.target.value)} className="mono" required />
                  </Field>
                  <Field label="内存" error={errs.memory}>
                    <Input aria-label="内存" value={memory} onChange={(e) => setMemory(e.target.value)} className="mono" required />
                  </Field>
                  <Field label="GPU" error={errs.gpu}>
                    <Input aria-label="GPU" value={gpu} onChange={(e) => setGpu(e.target.value)} className="mono" placeholder="0" />
                  </Field>
                </div>
                <Field label="Shell">
                  <Select aria-label="Shell" value={shell} onChange={(e) => setShell(e.target.value)}>
                    <option value="">镜像默认</option>
                    <option value="bash">bash</option>
                    <option value="zsh">zsh</option>
                    <option value="fish">fish</option>
                  </Select>
                </Field>
              </>
            )}
            {editing ? (
              <Field label="持久化">
                <div className="mono text-xs text-muted">{p.devpod.spec.persistence ? `${p.devpod.spec.persistence.size}（创建后不能修改）` : "无"}</div>
              </Field>
            ) : (
              !preset && (
                <Field label="持久化 home" hint="可选，例如 20Gi" error={errs.storage}>
                  <Input aria-label="持久化 home" value={persist} onChange={(e) => setPersist(e.target.value)} className="mono" />
                </Field>
              )
            )}
            {!editing && overlays.length > 0 && !preset && (
              <Field label="CPU 绑定（Kore）">
                <Select aria-label="CPU 绑定" value={overlay} onChange={(e) => setOverlay(e.target.value)}>
                  <option value="">不绑定</option>
                  {overlays.map((t) => (
                    <option key={t.metadata.name} value={t.metadata.name}>
                      {t.spec.displayName}
                    </option>
                  ))}
                </Select>
              </Field>
            )}
            <NodePicker value={node} onChange={setNode} />
            <Field label="协作者" hint="用户名，逗号分隔">
              <Input aria-label="协作者" value={collab} onChange={(e) => setCollab(e.target.value)} className="mono" />
            </Field>
          </>
        )}
        {rebuild && <Notice tone="warm">保存后会重建 Pod 并断开所有会话；未持久化的数据会丢失。</Notice>}
        {serverErr?.text && Object.keys(serverErr.fields).length === 0 && <Notice>{serverErr.text}</Notice>}
        <div className="flex justify-end gap-2 pt-1">
          <Button onClick={p.onClose}>取消</Button>
          <Button type="submit" variant="primary" disabled={busy || Object.keys(live).length > 0}>
            {editing ? "保存" : "创建"}
          </Button>
        </div>
      </form>
    </Drawer>
  );
}
