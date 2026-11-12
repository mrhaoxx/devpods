// 新建 DevPod (config-centric create spec §5.1): pick a config and fill
// in its variables — or write a YAML by hand. ?config= opens on a
// config, ?image= on a blank YAML with that image.
import { devpodHref, launchCluster, rememberLaunchCluster } from "../../data/cluster";
import { useClusterFilter, useClusterList } from "../../data/clusterFilter";
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
import { rememberConfig } from "./recentConfigs";

type Step = { kind: "pick" } | { kind: "launch"; name: string } | { kind: "yaml"; text: string; from: "blank" | "launch" };

function Launch({ cluster, name, onBack, onEditOnce }: { cluster?: string; name: string; onBack: () => void; onEditOnce: (yaml: string) => void }) {
  const nav = useNavigate();
  const user = useMe().data?.user ?? "";
  const q = useQuery({ queryKey: ["config", name], queryFn: () => getConfig(name), retry: false });
  if (q.isError) return <Notice>配置 {name} 不可用：它被删除，或不再分享给你。</Notice>;
  if (!q.data) return <p className="text-xs text-muted">加载中…</p>;
  return (
    <LaunchForm
      cluster={cluster}
      config={q.data}
      onBack={onBack}
      onEditOnce={onEditOnce}
      onDone={(r) => {
        // something was made from it: it was used
        if (r.items.some((i) => i.ok) && user) rememberConfig(user, name);
        if (r.items.some((i) => i.ok)) rememberLaunchCluster(cluster);
        // all made: go to what was made (leaving ?new=1 closes the
        // drawer); otherwise stay on the results
        if (r.items.length === 0 || !r.items.every((i) => i.ok)) return;
        if (r.group) nav(`/?group=${encodeURIComponent(r.group)}${cluster ? `&cluster=${encodeURIComponent(cluster)}` : ""}`);
        else nav(devpodHref({ cluster, name: r.items[0].name }));
      }}
    />
  );
}

export function NewDevPodDrawer({ config, image, in: asked, onClose }: { config?: string; image?: string; in?: string; onClose: () => void }) {
  const user = useMe().data?.user ?? "me";
  const nav = useNavigate();
  const clusters = useClusterList().data?.items ?? [];
  const { filter } = useClusterFilter();
  const [picked, setPicked] = useState<string>();
  const known = (c?: string) => !!c && clusters.some((x) => x.name === c);
  const cluster = known(picked) ? picked : known(asked) ? asked : launchCluster(clusters, filter);
  const shown = clusters.find((c) => c.name === cluster);
  const [step, setStep] = useState<Step>(() =>
    image ? { kind: "yaml", text: minimalYaml(user, image), from: "blank" } : config ? { kind: "launch", name: config } : { kind: "pick" },
  );
  return (
    <Drawer open onOpenChange={(o) => !o && onClose()} title="新建 DevPod" width={560}>
      {clusters.length >= 2 ? (
        <label className="mb-3 flex items-center gap-2 text-xs">
          <span className="text-muted">建在</span>
          <select
            aria-label="建在"
            value={cluster ?? ""}
            onChange={(e) => setPicked(e.target.value)}
            className="h-7 rounded-md border border-line bg-surface px-1 text-xs"
          >
            {clusters.map((c) => (
              <option key={c.name} value={c.name} disabled={!c.connected}>
                {(c.displayName || c.name) + (c.connected ? "" : "（离线）")}
              </option>
            ))}
          </select>
        </label>
      ) : (
        shown && (
          <p className="mb-3 text-xs text-muted">
            建在 <span className="text-ink">{shown.displayName || shown.name}</span>
          </p>
        )
      )}
      {step.kind === "pick" && (
        <ConfigPicker onPick={(c) => setStep({ kind: "launch", name: c.name })} onBlank={() => setStep({ kind: "yaml", text: minimalYaml(user), from: "blank" })} />
      )}
      {step.kind === "launch" && (
        <Launch
          key={`${step.name}@${cluster ?? ""}`}
          cluster={cluster}
          name={step.name}
          onBack={() => setStep({ kind: "pick" })}
          onEditOnce={(text) => setStep({ kind: "yaml", text, from: "launch" })}
        />
      )}
      {step.kind === "yaml" && (
        <BlankYaml
          cluster={cluster}
          initial={step.text}
          onBack={() => setStep({ kind: "pick" })}
          onCreated={(r) => {
            rememberLaunchCluster(r.cluster);
            nav(devpodHref(r));
          }}
        />
      )}
    </Drawer>
  );
}
