// The node a DevPod runs on (UI spec §5.6): the scheduler's choice, or
// one node. One the access rules bar, or that needs a reservation the
// user does not hold now, cannot be picked.
import type { NodeChoice } from "../../data/api";
import { useNodeChoices } from "../../data/queries";
import { Field, Select } from "../../design/Form";
import { fmtBytes, fmtCores } from "../../lib/format";
import { fmtTime } from "../../lib/reservations";

/** nodeLabel: one line describing a node in a picker. */
export function nodeLabel(n: NodeChoice): string {
  const bits = [n.name];
  if (n.gpuTotal > 0) bits.push(`${n.gpuModel ?? "GPU"} ×${n.gpuTotal}`);
  bits.push(`CPU ${fmtCores(n.cpu.allocated)}/${fmtCores(n.cpu.allocatable)}`, `内存 ${fmtBytes(n.mem.allocated)}/${fmtBytes(n.mem.allocatable)}`);
  if (!n.ready) bits.push("未就绪");
  if (n.mine) bits.push(`你的预约至 ${fmtTime(n.mine.until)}`);
  else if (n.access === "reservation") bits.push("需预约");
  else if (n.access === "denied") bits.push("无权限");
  return bits.join(" · ");
}

/** nodeWarning: what to say under the picker about the picked node. */
export function nodeWarning(n: NodeChoice | undefined): string | null {
  if (!n) return null;
  if (n.access === "denied") return `${n.name}：你没有使用这台机器的权限。`;
  if (n.access === "reservation") return `${n.name} 需要预约才能使用：先在「节点预约」里预约它，预约开始后再选。`;
  if (!n.ready) return `${n.name} 目前未就绪，DevPod 会一直等待。`;
  return null;
}

export function NodePicker({ value, onChange, hint }: { value: string; onChange: (node: string) => void; hint?: string }) {
  const nodes = useNodeChoices().data?.items ?? [];
  const picked = nodes.find((n) => n.name === value);
  const warn = nodeWarning(picked);
  return (
    <Field label="节点" hint={hint ?? "不指定时由调度器选择；指定后 DevPod 只在这台节点上运行"}>
      <Select aria-label="节点" value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">不指定（调度器选择）</option>
        {value && !picked && <option value={value}>{value}</option>}
        {nodes.map((n) => (
          <option key={n.name} value={n.name} disabled={!n.selectable && n.name !== value}>
            {nodeLabel(n)}
          </option>
        ))}
      </Select>
      {warn && (
        <span role="note" className="mt-1 block text-[11px] text-warm">
          {warn}
        </span>
      )}
    </Field>
  );
}
