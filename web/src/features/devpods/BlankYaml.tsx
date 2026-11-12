// 新建 DevPod from a YAML written by hand: a small DevPod to start from,
// or the YAML of one launch instance taken out to edit this one time.
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { ApiFailure, createDevPod } from "../../data/api";
import { Button } from "../../design/Button";
import { Notice } from "../../design/Form";
import { YamlEditor } from "../../design/yaml/YamlEditor";
import { yamlProblems } from "../../design/yaml/lint";

/** minimalYaml: the smallest DevPod worth starting from. */
export function minimalYaml(user: string, image = "ubuntu:24.04"): string {
  return `apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: ${user}-dev
spec:
  running: true
  pod:
    spec:
      containers:
        - name: dev
          image: ${image}
          resources:
            limits:
              cpu: "2"
              memory: 4Gi
`;
}

export function BlankYaml({ initial, onBack, onCreated }: { initial: string; onBack?: () => void; onCreated: (name: string) => void }) {
  const [text, setText] = useState(initial);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const nav = useNavigate();
  const qc = useQueryClient();
  const broken = yamlProblems(text).some((p) => p.severity === "error");
  const create = async () => {
    setBusy(true);
    setError(null);
    try {
      const dp = await createDevPod({ yaml: text });
      void qc.invalidateQueries({ queryKey: ["devpods"] });
      void qc.invalidateQueries({ queryKey: ["me"] });
      onCreated(dp.metadata.name);
    } catch (e) {
      setError(e instanceof ApiFailure ? e.body.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="space-y-3">
      <YamlEditor ariaLabel="DevPod YAML" value={text} onChange={setText} schema="devpod" minLines={16} maxLines={34} />
      {error && <Notice>创建失败：{error}</Notice>}
      <div className="flex justify-end gap-2">
        {onBack && <Button onClick={onBack}>上一步</Button>}
        <Button disabled={!text.trim()} onClick={() => nav("/configs/new", { state: { content: text } })} title="把这份 YAML 存成可复用、可分享的配置">
          保存为配置
        </Button>
        <Button variant="primary" disabled={busy || !text.trim() || broken} onClick={() => void create()}>
          创建
        </Button>
      </div>
    </div>
  );
}
