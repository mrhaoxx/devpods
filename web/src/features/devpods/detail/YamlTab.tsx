// A DevPod's manifest, to read and to edit as a whole. Saving replaces
// the spec; the server applies the rules of creating from YAML. The name
// and the owner are not the manifest's to change.
import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiFailure, getDevPodYaml, putDevPodYaml } from "../../../data/api";
import { qk } from "../../../data/queries";
import { Button } from "../../../design/Button";
import { EmptyState } from "../../../design/EmptyState";
import { Notice } from "../../../design/Form";
import { YamlEditor } from "../../../design/yaml/YamlEditor";
import { yamlProblems } from "../../../design/yaml/lint";
import { Panel } from "../../../design/Panel";

export function YamlTab({ name }: { name: string }) {
  const qc = useQueryClient();
  const key = ["devpod-yaml", name];
  const q = useQuery({ queryKey: key, queryFn: () => getDevPodYaml(name), retry: false });
  // base: what the server has; text: what is in the editor
  const [base, setBase] = useState<string | null>(null);
  const [text, setText] = useState("");
  const [saved, setSaved] = useState<null | { rebuild: boolean }>(null);
  const dirty = base !== null && text !== base;

  // the server's text comes in when nothing is being edited
  useEffect(() => {
    if (q.data && !dirty) {
      setBase(q.data.yaml);
      setText(q.data.yaml);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [q.data]);

  const save = useMutation({
    mutationFn: () => putDevPodYaml(name, text),
    onSuccess: (r) => {
      setBase(text);
      setSaved({ rebuild: r.rebuild });
      void qc.invalidateQueries({ queryKey: key });
      void qc.invalidateQueries({ queryKey: qk.devpod(name) });
      void qc.invalidateQueries({ queryKey: ["devpods"] });
      void qc.invalidateQueries({ queryKey: ["me"] });
    },
  });

  if (q.isError && base === null) {
    return <Notice>加载失败：{q.error instanceof ApiFailure ? q.error.body.message : String(q.error)}</Notice>;
  }
  if (base === null) return <EmptyState title="加载中…" />;

  // text that is not YAML is not sent: the editor says what is wrong with it
  const broken = dirty && yamlProblems(text).some((p) => p.severity === "error");
  return (
    <Panel
      title="YAML"
      actions={
        <>
          <Button
            disabled={!dirty || save.isPending}
            onClick={() => {
              setText(base);
              save.reset();
            }}
          >
            还原
          </Button>
          <Button
            variant="primary"
            disabled={!dirty || broken || save.isPending}
            title={broken ? "先改正 YAML 的语法错误" : undefined}
            onClick={() => {
              setSaved(null);
              save.mutate();
            }}
          >
            保存
          </Button>
        </>
      }
    >
      <div className="space-y-2 p-3">
        <YamlEditor
          ariaLabel="DevPod YAML"
          value={text}
          onChange={(t) => {
            setText(t);
            setSaved(null);
          }}
          minLines={16}
          maxLines={40}
        />
        {save.isError && <Notice>保存失败：{save.error instanceof ApiFailure ? save.error.body.message : String(save.error)}</Notice>}
        {saved && !dirty && (
          <p role="status" className="rounded-md bg-run-soft px-3 py-2 text-xs text-run">
            已保存。{saved.rebuild && "容器定义有变化，Pod 正在重建，已有的会话会断开。"}
          </p>
        )}
        {dirty && q.data?.running && <p className="text-[11px] text-warm">这个 DevPod 正在运行：改动容器定义（镜像、资源、挂载等）会重建 Pod 并断开所有会话。</p>}
        <p className="text-[11px] text-muted">
          保存会用这里的内容替换整个 spec，删掉的字段会被移除。名字和所有者不能改；状态、端口链接等由平台维护的内容不在这里。
        </p>
      </div>
    </Panel>
  );
}
