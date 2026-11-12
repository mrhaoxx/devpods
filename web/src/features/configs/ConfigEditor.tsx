// One config (config center spec §6.2): a new one, my own to edit, or
// someone else's to read. The variables of the YAML are listed as it is
// typed — the same reading as the server's.
import { useEffect, useMemo, useState, type FormEvent } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiFailure } from "../../data/api";
import { useMe } from "../../data/queries";
import { Button } from "../../design/Button";
import { EmptyState } from "../../design/EmptyState";
import { Field, Group, Input, Notice } from "../../design/Form";
import { YamlEditor } from "../../design/yaml/YamlEditor";
import { createConfig, getConfig, patchConfig, type Config, type ConfigPatch, type ConfigVar } from "./api";
import { LaunchDialog } from "./LaunchDialog";
import { INDEX, NAME, NODE, OWNER, parseVars } from "./vars";

/** What a page hands over to start a new config from. */
export type NewConfigState = { content?: string; title?: string; description?: string };

const MAX_NAME = 63;
const MAX_CONTENT = 64 << 10;
const idRE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const users = (s: string) => [...new Set(s.split(/[\s,，]+/).filter(Boolean))];

const builtinNote: Record<string, string> = {
  [OWNER]: "自动填入当前用户",
  [NAME]: "创建时填短名，自动加用户名前缀",
  [NODE]: "创建时选节点；选几个就创建几个",
  [INDEX]: "批量创建时的序号，从 1 开始",
};

function Variables({ vars }: { vars: ConfigVar[] }) {
  return (
    <div className="rounded-md border border-line bg-sunk p-2 text-xs">
      <div className="mb-1 font-medium">变量</div>
      {vars.length === 0 ? (
        <p className="text-faint">还没有变量</p>
      ) : (
        <ul aria-label="变量" className="space-y-1">
          {vars.map((v) => (
            <li key={v.name} className="flex flex-wrap items-baseline gap-x-2">
              <span className="mono text-ink">{v.name}</span>
              <span className="text-muted">{v.builtin ? builtinNote[v.name] : v.default !== undefined ? `默认 ${v.default}` : v.required ? "必填" : "可留空"}</span>
            </li>
          ))}
        </ul>
      )}
      <p className="mt-2 text-[11px] text-faint">
        写法：{"${VAR}"} 必填，{"${VAR:-默认值}"} 有默认值，{"$${"} 表示字面量 {"${"}。变量名是大写字母、数字和下划线；其他 {"${...}"}（如脚本里的 {"${home}"}）原样保留。
      </p>
    </div>
  );
}

export function ConfigEditor() {
  const { name } = useParams();
  const creating = !name;
  const me = useMe().data;
  const nav = useNavigate();
  const qc = useQueryClient();
  const handed = (useLocation().state ?? {}) as NewConfigState;
  const q = useQuery({ queryKey: ["config", name], queryFn: () => getConfig(name!), enabled: !creating, retry: false });
  const loaded = q.data;

  const [id, setId] = useState("");
  const [title, setTitle] = useState(creating ? (handed.title ?? "") : "");
  const [description, setDescription] = useState(creating ? (handed.description ?? "") : "");
  const [content, setContent] = useState(creating ? (handed.content ?? "") : "");
  const [share, setShare] = useState("");
  const [pub, setPub] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [launch, setLaunch] = useState(false);

  useEffect(() => {
    if (!loaded) return;
    setTitle(loaded.title);
    setDescription(loaded.description);
    setContent(loaded.content);
    setShare(loaded.sharedWith.join(", "));
    setPub(loaded.public);
  }, [loaded]);

  const vars = useMemo(() => parseVars(content), [content]);

  if (!creating && q.isError) {
    const gone = q.error instanceof ApiFailure && q.error.status === 404;
    return (
      <div className="p-4">
        <EmptyState
          tone={gone ? "neutral" : "error"}
          title={gone ? "没有这个配置，或它没有分享给你" : "加载失败"}
          hint={
            <Link to="/configs" className="text-accent hover:underline">
              ← 返回配置
            </Link>
          }
        />
      </div>
    );
  }
  if (!creating && !loaded) return <EmptyState title="加载中…" />;

  const mine = creating || !!loaded?.mine;
  const prefix = `${me?.user ?? ""}-`;
  const shownId = creating ? id : loaded!.name.startsWith(loaded!.owner + "-") ? loaded!.name.slice(loaded!.owner.length + 1) : loaded!.name;
  const idError = creating && id !== "" && (!idRE.test(id) || (prefix + id).length > MAX_NAME) ? `小写字母、数字和连字符；连同前缀不超过 ${MAX_NAME} 个字符` : undefined;
  const tooBig = new TextEncoder().encode(content).length > MAX_CONTENT;
  const list = users(share);
  const ok = !busy && title.trim() !== "" && content !== "" && !tooBig && (!creating || (id !== "" && !idError));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      if (creating) {
        await createConfig({ id, title: title.trim(), description, content, sharedWith: list, public: pub });
        void qc.invalidateQueries({ queryKey: ["configs"] });
        nav("/configs");
      } else {
        const was = loaded as Config;
        const body: ConfigPatch = {};
        if (title.trim() !== was.title) body.title = title.trim();
        if (description !== was.description) body.description = description;
        if (content !== was.content) body.content = content;
        if (list.join(",") !== was.sharedWith.join(",")) body.sharedWith = list;
        if (pub !== was.public) body.public = pub;
        const next = await patchConfig(was.name, body);
        qc.setQueryData(["config", name], next);
        void qc.invalidateQueries({ queryKey: ["configs"] });
        setSaved(true);
      }
    } catch (err) {
      setError(err instanceof ApiFailure ? (err.code === "CONFIG_EXISTS" ? `你已经有一个标识为 ${id} 的配置` : err.body.message) : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-3 p-3">
      <header className="flex flex-wrap items-center gap-2">
        <Link to="/configs" className="text-xs text-muted hover:text-accent">
          ← 配置
        </Link>
        <h1 className="text-sm font-semibold">{creating ? "新建配置" : loaded!.title}</h1>
        {!creating && <span className="mono text-xs text-muted">{loaded!.name}</span>}
        <span className="flex-1" />
        {!creating && <Button onClick={() => setLaunch(true)}>用此配置创建</Button>}
        {!creating && (
          <Button onClick={() => nav("/configs/new", { state: { content, title, description } satisfies NewConfigState })}>另存为副本</Button>
        )}
        {mine && (
          <Button type="submit" variant="primary" disabled={!ok}>
            保存
          </Button>
        )}
      </header>
      {!mine && (
        <Notice tone="idle">
          这是 {loaded!.owner} 的配置，你可以用它创建 DevPod 或另存为自己的副本，不能修改原件。创建时按你自己的权限和配额检查。
        </Notice>
      )}
      {error && <Notice>{error}</Notice>}
      {saved && <Notice tone="run">已保存。</Notice>}
      <div className="grid gap-3 lg:grid-cols-[minmax(0,1fr)_18rem]">
        <div className="space-y-3">
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="标题">
              <Input aria-label="标题" value={title} onChange={(e) => setTitle(e.target.value)} maxLength={80} readOnly={!mine} />
            </Field>
            <Field label="标识" hint={creating ? "保存后不能修改" : undefined} error={idError}>
              <div className="flex items-center rounded-md border border-line-strong bg-surface focus-within:border-accent">
                <span className="mono pl-2.5 text-sm text-faint">{creating ? prefix : `${loaded!.owner}-`}</span>
                <input
                  aria-label="标识"
                  value={shownId}
                  onChange={(e) => setId(e.target.value)}
                  readOnly={!creating}
                  className="mono h-8 w-full bg-transparent pr-2.5 text-sm focus:outline-none read-only:text-muted"
                />
              </div>
            </Field>
          </div>
          <Field label="说明">
            <Input aria-label="说明" value={description} onChange={(e) => setDescription(e.target.value)} maxLength={500} readOnly={!mine} />
          </Field>
          <Group label="YAML" hint="完整的 DevPod YAML，可以含变量" error={tooBig ? "内容超过 64 KiB" : undefined}>
            <YamlEditor ariaLabel="YAML" value={content} onChange={setContent} readOnly={!mine} minLines={24} maxLines={44} schema="devpod" />
          </Group>
        </div>
        <div className="space-y-3">
          <Variables vars={vars} />
          {mine && (
            <div className="space-y-3 rounded-md border border-line p-2">
              <Field label="分享给" hint="用户名，逗号或空格分隔">
                <Input aria-label="分享给" value={share} onChange={(e) => setShare(e.target.value)} className="mono" placeholder="alice, bob" />
              </Field>
              <label className="flex items-start gap-1.5 text-xs">
                <input type="checkbox" aria-label="公开" checked={pub} onChange={(e) => setPub(e.target.checked)} className="mt-0.5" />
                <span>
                  公开
                  <span className="block text-[11px] text-faint">所有登录用户都能看到并使用。</span>
                </span>
              </label>
              <p className="text-[11px] text-faint">被分享的人只能查看、使用和另存为副本。他们创建的 DevPod 归他们自己，按他们自己的权限和配额检查。</p>
            </div>
          )}
        </div>
      </div>
      {launch && loaded && <LaunchDialog config={{ name: loaded.name, title: loaded.title, variables: parseVars(loaded.content) }} onClose={() => setLaunch(false)} />}
    </form>
  );
}
