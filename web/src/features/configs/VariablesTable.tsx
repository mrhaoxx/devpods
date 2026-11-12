// What a config says of its variables (config-centric create spec §5.4):
// a row per variable its text has; the built-in ones say what fills
// them, my own take a type (and a choice its options) and a description.
// Defaults stay in the text: ${X:-default}. What is said of a variable
// the text no longer has is kept while editing (a half-typed ${GPU:-1
// is not the end of GPU) and left out when saved (keptDecls).
import { useMemo, useState } from "react";
import type { ParsedVar, VarDecl, VarType } from "./api";
import { COUNT, INDEX, LEADER, NAME, NODE, OWNER, parseVars } from "./vars";

const builtinNote: Record<string, string> = {
  [OWNER]: "自动填入当前用户",
  [NAME]: "创建时填短名，自动加用户名前缀",
  [NODE]: "创建时选节点；选几个就创建几个",
  [INDEX]: "一次创建多个时的序号，从 0 开始",
  [COUNT]: "这一次创建的数量",
  [LEADER]: "这一次第 0 个的名字（可作 MASTER_ADDR）",
};

const types: [VarType, string][] = [
  ["string", "文本"],
  ["int", "整数"],
  ["choice", "选项"],
  ["node", "节点"],
  ["image", "镜像"],
];

/** cleanDecl: a declaration with only what it says; null when it says nothing. */
export function cleanDecl(d: VarDecl): VarDecl | null {
  const out: VarDecl = { name: d.name };
  if (d.type && d.type !== "string") out.type = d.type;
  if (d.description?.trim()) out.description = d.description;
  if (d.type === "choice" && d.options?.length) out.options = d.options;
  return Object.keys(out).length > 1 ? out : null;
}

/** keptDecls: what is said of the variables the text has, in its order — what is saved. */
export function keptDecls(content: string, decls: VarDecl[]): VarDecl[] {
  return parseVars(content)
    .filter((v) => !v.builtin)
    .map((v) => decls.find((d) => d.name === v.name))
    .filter((d): d is VarDecl => d !== undefined);
}

const need = (v: ParsedVar) => (v.builtin ? builtinNote[v.name] : v.default !== undefined ? `默认 ${v.default}` : v.required ? "必填" : "可留空");

export function VariablesTable({ content, decls, onChange, readOnly = false }: { content: string; decls: VarDecl[]; onChange: (d: VarDecl[]) => void; readOnly?: boolean }) {
  const vars = useMemo(() => parseVars(content), [content]);
  const own = useMemo(() => vars.filter((v) => !v.builtin).map((v) => v.name), [vars]);
  const [optionText, setOptionText] = useState<Record<string, string>>({});

  const declOf = (name: string): VarDecl => decls.find((d) => d.name === name) ?? { name };
  const update = (name: string, patch: Partial<VarDecl>) => {
    const others = decls.filter((d) => d.name !== name);
    const changed = cleanDecl({ ...declOf(name), ...patch });
    onChange(changed ? [...others, changed] : others);
  };

  return (
    <div className="rounded-md border border-line bg-sunk p-2 text-xs">
      <div className="mb-1 font-medium">变量</div>
      {vars.length === 0 ? (
        <p className="text-faint">还没有变量</p>
      ) : (
        <ul aria-label="变量" className="space-y-2">
          {vars.map((v) => {
            const d = declOf(v.name);
            const type = d.type ?? "string";
            return (
              <li key={v.name} className="space-y-1">
                <div className="flex flex-wrap items-baseline gap-x-2">
                  <span className="mono text-ink">{v.name}</span>
                  <span className="text-muted">{need(v)}</span>
                </div>
                {!v.builtin &&
                  (readOnly ? (
                    (type !== "string" || d.description) && (
                      <div className="text-[11px] text-muted">
                        {types.find(([t]) => t === type)?.[1]}
                        {d.options?.length ? `：${d.options.join("、")}` : ""}
                        {d.description ? ` · ${d.description}` : ""}
                      </div>
                    )
                  ) : (
                    <div className="space-y-1">
                      <div className="flex gap-1">
                        <select
                          aria-label={`${v.name} 的类型`}
                          value={type}
                          onChange={(e) => update(v.name, { type: e.target.value as VarType })}
                          className="h-6 rounded border border-line bg-surface px-1 text-[11px]"
                        >
                          {types.map(([t, label]) => (
                            <option key={t} value={t}>
                              {label}
                            </option>
                          ))}
                        </select>
                        <input
                          aria-label={`${v.name} 的说明`}
                          placeholder="说明（可选）"
                          value={d.description ?? ""}
                          maxLength={200}
                          onChange={(e) => update(v.name, { description: e.target.value })}
                          className="h-6 min-w-0 flex-1 rounded border border-line bg-surface px-1.5 text-[11px]"
                        />
                      </div>
                      {type === "choice" && (
                        <input
                          aria-label={`${v.name} 的选项`}
                          placeholder="选项，逗号分隔"
                          value={optionText[v.name] ?? (d.options ?? []).join(", ")}
                          onChange={(e) => {
                            setOptionText((m) => ({ ...m, [v.name]: e.target.value }));
                            update(v.name, { options: e.target.value.split(/[,，]/).map((x) => x.trim()).filter(Boolean) });
                          }}
                          className="mono h-6 w-full rounded border border-line bg-surface px-1.5 text-[11px]"
                        />
                      )}
                    </div>
                  ))}
              </li>
            );
          })}
        </ul>
      )}
      <p className="mt-2 text-[11px] text-faint">
        写法：{"${VAR}"} 必填，{"${VAR:-默认值}"} 有默认值，{"$${"} 表示字面量 {"${"}。变量名是大写字母、数字和下划线；其他 {"${...}"}（如脚本里的 {"${home}"}）原样保留。
      </p>
    </div>
  );
}
