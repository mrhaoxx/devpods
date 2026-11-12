// One variable of a config in the launch form: a control by its type
// (config-centric create spec §5.1), its description beside it.
import { useNodeChoices } from "../../data/queries";
import { Field, Input, Select } from "../../design/Form";
import { MY_IMAGES_LIST, MyImagesList } from "../images/MyImages";
import { nodeLabel } from "../devpods/NodePicker";
import type { ConfigVar } from "./api";

function hintOf(v: ConfigVar): string {
  return v.required ? "必填" : v.default ? `默认 ${v.default}` : "可留空";
}

function NodeSelect({ v, value, onChange }: { v: ConfigVar; value: string; onChange: (s: string) => void }) {
  const nodes = useNodeChoices().data?.items ?? [];
  return (
    <Select aria-label={v.name} value={value} onChange={(e) => onChange(e.target.value)} className="mono">
      <option value="">{v.required ? "选择节点…" : "不指定"}</option>
      {nodes.map((n) => (
        <option key={n.name} value={n.name} disabled={!n.selectable}>
          {nodeLabel(n)}
        </option>
      ))}
    </Select>
  );
}

export function VarInput({ v, value, onChange }: { v: ConfigVar; value: string; onChange: (s: string) => void }) {
  let control;
  switch (v.type) {
    case "int":
      control = <Input aria-label={v.name} type="number" step={1} value={value} onChange={(e) => onChange(e.target.value)} className="mono w-32" />;
      break;
    case "choice":
      control = (
        <Select aria-label={v.name} value={value} onChange={(e) => onChange(e.target.value)} className="mono">
          {(v.required || !v.default) && <option value="">选择…</option>}
          {(v.options ?? []).map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </Select>
      );
      break;
    case "node":
      control = <NodeSelect v={v} value={value} onChange={onChange} />;
      break;
    case "image":
      control = (
        <>
          <Input aria-label={v.name} value={value} onChange={(e) => onChange(e.target.value)} className="mono" list={MY_IMAGES_LIST} spellCheck={false} />
          <MyImagesList />
        </>
      );
      break;
    default:
      control = <Input aria-label={v.name} value={value} onChange={(e) => onChange(e.target.value)} className="mono" spellCheck={false} />;
  }
  return (
    <Field label={v.name} hint={hintOf(v)}>
      {control}
      {v.description && <span className="mt-0.5 block text-[11px] text-muted">{v.description}</span>}
    </Field>
  );
}
