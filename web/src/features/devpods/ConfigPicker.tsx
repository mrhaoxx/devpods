// 新建 DevPod, first step (config-centric create spec §5.1): the configs I
// may use — official, mine, shared with me, public — or a blank YAML.
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Button } from "../../design/Button";
import { Input } from "../../design/Form";
import { listConfigs, type ConfigSummary } from "../configs/api";

type Group = { label: string; items: ConfigSummary[] };

/** pickerGroups: each config once, in the first group it belongs to. */
export function pickerGroups(items: ConfigSummary[]): Group[] {
  const groups: Group[] = [
    { label: "官方", items: [] },
    { label: "我的", items: [] },
    { label: "分享给我的", items: [] },
    { label: "公开", items: [] },
  ];
  for (const c of items) {
    if (c.official) groups[0].items.push(c);
    else if (c.mine) groups[1].items.push(c);
    else if (!c.public) groups[2].items.push(c);
    else groups[3].items.push(c);
  }
  return groups.filter((g) => g.items.length > 0);
}

const matches = (c: ConfigSummary, q: string) => {
  const s = q.trim().toLowerCase();
  return !s || [c.title, c.name, c.owner, c.description].some((x) => x.toLowerCase().includes(s));
};

export function ConfigPicker({ onPick, onBlank }: { onPick: (c: ConfigSummary) => void; onBlank: () => void }) {
  const q = useQuery({ queryKey: ["configs", "all"], queryFn: () => listConfigs() });
  const [search, setSearch] = useState("");
  const all = q.data?.items ?? [];
  const groups = useMemo(() => pickerGroups(all.filter((c) => matches(c, search))), [all, search]);
  return (
    <div className="space-y-3">
      <div className="flex gap-2">
        <Input aria-label="搜索配置" placeholder="搜索配置…" value={search} onChange={(e) => setSearch(e.target.value)} className="flex-1" />
        <Button onClick={onBlank} title="自己写一份 DevPod YAML">
          空白 YAML
        </Button>
      </div>
      {q.isError && <p className="text-xs text-fail">配置列表加载失败。</p>}
      {q.isSuccess && all.length === 0 && (
        <p className="rounded-md bg-sunk px-3 py-3 text-xs text-muted">
          还没有可用的配置。可以用「空白 YAML」自己写一份，或到
          <Link to="/configs" className="mx-0.5 text-accent hover:underline">
            配置
          </Link>
          页新建。
        </p>
      )}
      <section aria-label="配置列表" className="space-y-3">
        {groups.map((g) => (
          <div key={g.label}>
            <h3 className="mb-1 text-xs font-semibold text-muted">{g.label}</h3>
            <ul className="space-y-1">
              {g.items.map((c) => {
                const vars = c.variables.filter((v) => !["OWNER", "INDEX", "COUNT", "LEADER"].includes(v.name)).map((v) => v.name);
                return (
                  <li key={c.name}>
                    <button type="button" onClick={() => onPick(c)} className="w-full rounded-md border border-line px-3 py-2 text-left hover:border-accent hover:bg-sunk">
                      <div className="flex items-baseline gap-2">
                        <span className="truncate text-sm font-medium">{c.title}</span>
                        {c.official && <span className="rounded bg-accent-soft px-1 text-[10px] text-accent">官方</span>}
                        <span className="mono ml-auto shrink-0 text-[11px] text-faint">{c.owner}</span>
                      </div>
                      {c.description && <div className="mt-0.5 truncate text-xs text-muted">{c.description}</div>}
                      {vars.length > 0 && <div className="mono mt-0.5 truncate text-[11px] text-faint">{vars.join(" · ")}</div>}
                    </button>
                  </li>
                );
              })}
            </ul>
          </div>
        ))}
        {q.isSuccess && all.length > 0 && groups.length === 0 && <p className="text-xs text-muted">没有符合的配置。</p>}
      </section>
    </div>
  );
}
