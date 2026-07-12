import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { me, listTemplates, createDevPod, ApiFailure, Template } from "../api";

type Mode = "preset" | "custom" | "yaml";

function TemplateCard({ tpl, selected, onClick }: { tpl: Template; selected: boolean; onClick: () => void }) {
  const b = tpl.spec.binding;
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-lg border p-3 text-left text-sm ${selected ? "border-blue-600 ring-1 ring-blue-600" : "hover:border-slate-400"}`}
    >
      <div className="font-medium">{tpl.spec.displayName}</div>
      {tpl.spec.description && <div className="text-xs text-slate-500">{tpl.spec.description}</div>}
      {b && (
        <div className="mt-1 text-xs text-slate-600">
          {b.annotations["kore.zjusct.io/pin"] === "true"
            ? `pinned · ${b.annotations["kore.zjusct.io/numa-policy"] ?? "single"} NUMA`
            : `pool ${b.annotations["kore.zjusct.io/pool"]} (${b.annotations["kore.zjusct.io/pool-size"]} cores)`}
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
    <main className="mx-auto max-w-2xl p-8">
      <h1 className="mb-6 text-xl font-semibold">New DevPod</h1>
      <div className="mb-4 flex gap-2 text-sm">
        {(["preset", "custom", "yaml"] as Mode[]).map((m) => (
          <button
            key={m}
            onClick={() => {
              setMode(m);
              setTplRef("");
            }}
            className={`rounded px-3 py-1 ${mode === m ? "bg-blue-600 text-white" : "border"}`}
          >
            {m === "preset" ? "Preset" : m === "custom" ? "Custom" : "YAML"}
          </button>
        ))}
      </div>

      <form onSubmit={submit} className="space-y-4">
        {mode !== "yaml" && (
          <label className="block text-sm">
            Name suffix ({budget - name.length} chars left)
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              maxLength={budget}
              className="mt-1 w-full rounded border px-2 py-1"
              required
            />
            <span className="text-xs text-slate-400">
              {meQ.data?.user}-{name || "…"}
            </span>
          </label>
        )}

        {mode === "preset" && (
          <div className="grid grid-cols-2 gap-2">
            {presets.map((t) => (
              <TemplateCard
                key={t.metadata.name}
                tpl={t}
                selected={tplRef === t.metadata.name}
                onClick={() => setTplRef(t.metadata.name)}
              />
            ))}
            {presets.length === 0 && <p className="col-span-2 text-sm text-slate-400">No presets published.</p>}
          </div>
        )}

        {mode === "custom" && (
          <>
            <label className="block text-sm">
              Image
              <input
                value={image}
                onChange={(e) => setImage(e.target.value)}
                className="mt-1 w-full rounded border px-2 py-1"
                required
              />
            </label>
            <div className="grid grid-cols-2 gap-3">
              <label className="block text-sm">
                CPU limit
                <input
                  value={cpu}
                  onChange={(e) => setCpu(e.target.value)}
                  className="mt-1 w-full rounded border px-2 py-1"
                  required
                />
              </label>
              <label className="block text-sm">
                Memory limit
                <input
                  value={memory}
                  onChange={(e) => setMemory(e.target.value)}
                  className="mt-1 w-full rounded border px-2 py-1"
                  required
                />
              </label>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <label className="block text-sm">
                Shell (optional)
                <select
                  value={shell}
                  onChange={(e) => setShell(e.target.value)}
                  className="mt-1 w-full rounded border px-2 py-1"
                >
                  <option value="">image default</option>
                  <option>bash</option>
                  <option>zsh</option>
                  <option>fish</option>
                </select>
              </label>
              <label className="block text-sm">
                Home volume (optional, e.g. 20Gi)
                <input
                  value={persist}
                  onChange={(e) => setPersist(e.target.value)}
                  className="mt-1 w-full rounded border px-2 py-1"
                />
              </label>
            </div>
            {overlays.length > 0 && (
              <fieldset className="text-sm">
                <legend className="mb-1">CPU binding (admin-curated)</legend>
                <div className="grid grid-cols-2 gap-2">
                  <button
                    type="button"
                    onClick={() => setTplRef("")}
                    className={`rounded-lg border p-3 text-left ${tplRef === "" ? "border-blue-600 ring-1 ring-blue-600" : ""}`}
                  >
                    <div className="font-medium">None</div>
                    <div className="text-xs text-slate-500">shared cores</div>
                  </button>
                  {overlays.map((t) => (
                    <TemplateCard
                      key={t.metadata.name}
                      tpl={t}
                      selected={tplRef === t.metadata.name}
                      onClick={() => setTplRef(t.metadata.name)}
                    />
                  ))}
                </div>
              </fieldset>
            )}
          </>
        )}

        {mode === "yaml" && (
          <textarea
            value={yamlText}
            onChange={(e) => setYamlText(e.target.value)}
            rows={16}
            className="w-full rounded border p-2 font-mono text-xs"
            placeholder={"apiVersion: devpod.io/v1alpha1\nkind: DevPod\n..."}
          />
        )}

        {err && <p className="rounded bg-red-50 p-3 text-sm text-red-700">{err}</p>}
        <button
          className="rounded bg-blue-600 px-4 py-2 text-sm text-white disabled:opacity-50"
          type="submit"
          disabled={mode === "preset" && !tplRef}
        >
          Create
        </button>
      </form>
    </main>
  );
}
