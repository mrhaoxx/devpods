// 新建 DevPod (config-centric create spec §5.1): pick a config and fill
// in its variables — or write a YAML by hand. ?config= opens on a
// config, ?image= on a blank YAML with that image.
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { useMe } from "../../data/queries";
import { Drawer } from "../../design/Drawer";
import { Notice } from "../../design/Form";
import { getConfig } from "../configs/api";
import { LaunchForm } from "../configs/LaunchForm";
import { BlankYaml, minimalYaml } from "./BlankYaml";
import { ConfigPicker } from "./ConfigPicker";

type Step = { kind: "pick" } | { kind: "launch"; name: string } | { kind: "yaml"; text: string; from: "blank" | "launch" };

function Launch({ name, onBack, onEditOnce }: { name: string; onBack: () => void; onEditOnce: (yaml: string) => void }) {
  const nav = useNavigate();
  const q = useQuery({ queryKey: ["config", name], queryFn: () => getConfig(name), retry: false });
  if (q.isError) return <Notice>配置 {name} 不可用：它被删除，或不再分享给你。</Notice>;
  if (!q.data) return <p className="text-xs text-muted">加载中…</p>;
  return (
    <LaunchForm
      config={q.data}
      onBack={onBack}
      onEditOnce={onEditOnce}
      onDone={(r) => {
        // all made: go to what was made (leaving ?new=1 closes the
        // drawer); otherwise stay on the results
        if (r.items.length === 0 || !r.items.every((i) => i.ok)) return;
        if (r.group) nav(`/?group=${encodeURIComponent(r.group)}`);
        else nav(`/devpods/${r.items[0].name}`);
      }}
    />
  );
}

export function NewDevPodDrawer({ config, image, onClose }: { config?: string; image?: string; onClose: () => void }) {
  const user = useMe().data?.user ?? "me";
  const nav = useNavigate();
  const [step, setStep] = useState<Step>(() =>
    image ? { kind: "yaml", text: minimalYaml(user, image), from: "blank" } : config ? { kind: "launch", name: config } : { kind: "pick" },
  );
  return (
    <Drawer open onOpenChange={(o) => !o && onClose()} title="新建 DevPod" width={560}>
      {step.kind === "pick" && (
        <ConfigPicker onPick={(c) => setStep({ kind: "launch", name: c.name })} onBlank={() => setStep({ kind: "yaml", text: minimalYaml(user), from: "blank" })} />
      )}
      {step.kind === "launch" && (
        <Launch
          key={step.name}
          name={step.name}
          onBack={() => setStep({ kind: "pick" })}
          onEditOnce={(text) => setStep({ kind: "yaml", text, from: "launch" })}
        />
      )}
      {step.kind === "yaml" && (
        <BlankYaml
          initial={step.text}
          onBack={() => setStep({ kind: "pick" })}
          onCreated={(name) => nav(`/devpods/${name}`)}
        />
      )}
    </Drawer>
  );
}
