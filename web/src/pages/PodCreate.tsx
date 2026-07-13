import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { me, listTemplates, createDevPod, ApiFailure, Template } from "../api";
import { BackLink, Button, CoreMeter, Field, Input, Notice, Shell, cx } from "../ui";

type Mode = "preset" | "custom" | "yaml";

function TemplateTile({ tpl, selected, onClick }: { tpl: Template; selected: boolean; onClick: () => void }) {
  const b = tpl.spec.binding;
  const pinned = b?.annotations["kore.zjusct.io/pin"] === "true";
  return (
    <button
      type="button"
      onClick={onClick}
      className={cx(
        "rounded-xl border p-3 text-left transition-colors",
        selected ? "border-accent bg-accent-soft" : "border-line bg-surface hover:border-line-strong",
      )}
    >
      <div className="text-sm font-medium text-ink">{tpl.spec.displayName}</div>
      {tpl.spec.description && <div className="mt-0.5 text-xs text-muted">{tpl.spec.description}</div>}
      {b && (
        <div className="mt-2 flex items-center gap-2">
          {pinned ? <CoreMeter used={0} limit={8} /> : null}
          <span className="mono text-[11px] text-faint">
            {pinned
              ? `pinned · ${b.annotations["kore.zjusct.io/numa-policy"] ?? "single"} numa`
              : `pool ${b.annotations["kore.zjusct.io/pool"]} · ${b.annotations["kore.zjusct.io/pool-size"]} cores`}
          </span>
        </div>
      )}
    </button>
  );
}

export default function PodCreate() {
  const nav = useNavigate();
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const tplQ = useQuery({ queryKey: ["templates"], queryFn: listTemplates });
  const [mode, setMode] = useState<Mode>("custom");
  const [name, setName] = useState("");
  const [image, setImage] = useState("ubuntu:24.04");
  const [cpu, setCpu] = useState("2");
  const [memory, setMemory] = useState("4Gi");
  const [shell, setShell] = useState("");
  const [persist, setPersist] = useState("");
  const [tplRef, setTplRef] = useState("");
  const [yamlText, setYamlText] = useState("");
  const [err, setErr] = useState<string | null>(null);

  const templates = tplQ.data?.items ?? [];
  const presets = templates.filter((t) => t.spec.podPreset);
  const overlays = templates.filter((t) => t.spec.binding && !t.spec.podPreset);
  const budget = meQ.data?.nameBudget ?? 21;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr(null);
    try {
      let body: Record<string, unknown>;
      if (mode === "yaml") {
        body = { yaml: yamlText };
        if (tplRef) body.templateRef = tplRef;
      } else if (mode === "preset") {
        body = { name, templateRef: tplRef };
      } else {
        body = { name, image, cpu, memory };
        if (shell) body.shell = shell;
        if (persist) body.persistence = { size: persist, mountPath: "/home/dev" };
        if (tplRef) body.templateRef = tplRef;
      }
      const dp = await createDevPod(body);
      nav(`/devpods/${dp.metadata.name}`);
    } catch (e) {
      setErr(
        e instanceof ApiFailure
          ? `${e.body.message}${e.body.detail ? ` — ${JSON.stringify(e.body.detail)}` : ""}`
          : String(e),
      );
    }
  };

  return (
    <Shell>
      <BackLink />
      <h1 className="mono mb-5 mt-3 text-xl font-semibold tracking-tight">New DevPod</h1>

      <div className="mb-5 inline-flex rounded-lg border border-line bg-sunk p-0.5 text-sm">
        {(["preset", "custom", "yaml"] as Mode[]).map((m) => (
          <button
            key={m}
            onClick={() => {
              setMode(m);
              setTplRef("");
            }}
            className={cx(
              "rounded-md px-3 py-1.5 font-medium transition-colors",
              mode === m ? "bg-surface text-ink shadow-sm" : "text-muted hover:text-ink",
            )}
          >
            {m === "preset" ? "Preset" : m === "custom" ? "Custom" : "YAML"}
          </button>
        ))}
      </div>

      <form onSubmit={submit} className="space-y-5">
        {mode !== "yaml" && (
          <Field label="Name" hint={`${budget - name.length} left`}>
            <div className="flex items-center rounded-lg border border-line-strong bg-surface focus-within:border-accent">
              <span className="mono py-2 pl-3 text-sm text-faint">{meQ.data?.user}-</span>
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                maxLength={budget}
                className="mono w-full bg-transparent py-2 pr-3 text-sm text-ink focus:outline-none"
                required
              />
            </div>
          </Field>
        )}

        {mode === "preset" && (
          <div className="grid gap-2 sm:grid-cols-2">
            {presets.map((t) => (
              <TemplateTile key={t.metadata.name} tpl={t} selected={tplRef === t.metadata.name} onClick={() => setTplRef(t.metadata.name)} />
            ))}
            {presets.length === 0 && <p className="text-sm text-faint">No presets published.</p>}
          </div>
        )}

        {mode === "custom" && (
          <>
            <Field label="Image">
              <Input value={image} onChange={(e) => setImage(e.target.value)} className="mono" required />
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="CPU limit">
                <Input value={cpu} onChange={(e) => setCpu(e.target.value)} className="mono" required />
              </Field>
              <Field label="Memory limit">
                <Input value={memory} onChange={(e) => setMemory(e.target.value)} className="mono" required />
              </Field>
            </div>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="Shell" hint="optional">
                <select
                  value={shell}
                  onChange={(e) => setShell(e.target.value)}
                  className="w-full rounded-lg border border-line-strong bg-surface px-3 py-2 text-sm text-ink focus:border-accent focus-visible:outline-none"
                >
                  <option value="">image default</option>
                  <option>bash</option>
                  <option>zsh</option>
                  <option>fish</option>
                </select>
              </Field>
              <Field label="Home volume" hint="optional">
                <Input value={persist} onChange={(e) => setPersist(e.target.value)} placeholder="e.g. 20Gi" className="mono" />
              </Field>
            </div>
            {overlays.length > 0 && (
              <div>
                <p className="eyebrow mb-2">CPU binding · admin-curated</p>
                <div className="grid gap-2 sm:grid-cols-2">
                  <button
                    type="button"
                    onClick={() => setTplRef("")}
                    className={cx(
                      "rounded-xl border p-3 text-left transition-colors",
                      tplRef === "" ? "border-accent bg-accent-soft" : "border-line bg-surface hover:border-line-strong",
                    )}
                  >
                    <div className="text-sm font-medium text-ink">None</div>
                    <div className="mt-0.5 text-xs text-muted">shared cores</div>
                  </button>
                  {overlays.map((t) => (
                    <TemplateTile key={t.metadata.name} tpl={t} selected={tplRef === t.metadata.name} onClick={() => setTplRef(t.metadata.name)} />
                  ))}
                </div>
              </div>
            )}
          </>
        )}

        {mode === "yaml" && (
          <textarea
            value={yamlText}
            onChange={(e) => setYamlText(e.target.value)}
            rows={16}
            spellCheck={false}
            className="mono w-full rounded-lg border border-line-strong bg-sunk p-3 text-xs text-ink focus:border-accent focus-visible:outline-none"
            placeholder={"apiVersion: devpod.io/v1alpha1\nkind: DevPod\n..."}
          />
        )}

        {err && <Notice>{err}</Notice>}
        <Button type="submit" variant="accent" disabled={mode === "preset" && !tplRef}>
          Create environment
        </Button>
      </form>
    </Shell>
  );
}
