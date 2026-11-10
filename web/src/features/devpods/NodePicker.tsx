// The node a DevPod runs on (UI spec §5.6): the scheduler's choice, or
// one node. A node someone else holds right now cannot be picked; one
// with an upcoming reservation by someone else can, with a warning.
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
  if (n.held) bits.push(n.selectable ? `你的预约至 ${fmtTime(n.held.until)}` : `${n.held.user} 预约至 ${fmtTime(n.held.until)}`);
  return bits.join(" · ");
}

/** nodeWarning: what to say under the picker about the picked node. */
export function nodeWarning(n: NodeChoice | undefined): string | null {
  if (!n) return null;
  if (!n.selectable && n.held) return `${n.name} 正被 ${n.held.user} 预约至 ${fmtTime(n.held.until)}，现在不能使用。`;
  if (n.next) return `${n.name} 将于 ${fmtTime(n.next.start)} 被 ${n.next.user} 预约，届时这个 DevPod 会被自动休眠。`;
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
