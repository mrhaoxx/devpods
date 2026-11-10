import { Link } from "react-router-dom";
import { SessionSummary } from "../api";
import { fmtBytes, fmtDuration } from "../lib/format";
import { cx } from "../ui";

function fmtStart(iso: string): string {
  const d = new Date(iso);
  const today = new Date();
  const time = d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  return d.toDateString() === today.toDateString() ? time : `${d.toLocaleDateString()} ${time}`;
}

function Tag({ tone, children }: { tone: "warn" | "fail" | "muted"; children: React.ReactNode }) {
  return (
    <span
      className={cx(
        "ml-1 rounded border px-1 text-[9.5px]",
        tone === "warn" && "border-warm/30 bg-warm-soft text-warm",
        tone === "fail" && "border-fail/30 bg-fail-soft text-fail",
        tone === "muted" && "border-line text-muted",
      )}
    >
      {children}
    </span>
  );
}

// SessionsTable lists audit sessions (DevPod detail tab and audit page).
export function SessionsTable({ items, showDevPod, now }: { items: SessionSummary[]; showDevPod?: boolean; now: Date }) {
  const cell = "whitespace-nowrap px-2 py-1";
  return (
    <div className="mono overflow-hidden rounded-lg border border-line bg-surface text-xs">
      <table className="w-full border-collapse">
        <thead>
          <tr className="border-b border-line bg-sunk text-left text-[10.5px] uppercase tracking-wider text-faint">
            <th className="px-2 py-1.5 font-medium">User</th>
            {showDevPod && <th className="px-2 py-1.5 font-medium">DevPod</th>}
            <th className="px-2 py-1.5 font-medium">From</th>
            <th className="px-2 py-1.5 font-medium">Start</th>
            <th className="px-2 py-1.5 font-medium">Dur</th>
            <th className="px-2 py-1.5 font-medium">Cmds</th>
            <th className="px-2 py-1.5 font-medium">Files</th>
            <th className="px-2 py-1.5 font-medium">↓/↑</th>
            <th className="px-2 py-1.5 font-medium" />
          </tr>
        </thead>
        <tbody>
          {items.map((s) => {
            const open = !s.closed_at;
            const end = s.closed_at ? new Date(s.closed_at) : now;
            return (
              <tr key={s.session_id} className="h-[30px] border-b border-line/60 last:border-0 hover:bg-sunk/50">
                <td className={cell}>
                  <span className={cx("mr-1.5 inline-block size-2 rounded-[2px]", open ? "bg-run pulse" : "bg-idle")} />
                  {s.user}
                </td>
                {showDevPod && <td className={cx(cell, "text-muted")}>{s.devpod}</td>}
                <td className={cx(cell, "text-muted")}>{s.client_ip.replace(/:\d+$/, "")}</td>
                <td className={cx(cell, "text-muted")}>{fmtStart(s.opened_at)}</td>
                <td className={cell}>{fmtDuration(end.getTime() - new Date(s.opened_at).getTime())}</td>
                <td className={cell}>{s.commands}</td>
                <td className={cell}>{s.files || <span className="text-faint">—</span>}</td>
                <td className={cx(cell, "text-muted")}>
                  {fmtBytes(s.bytes_out)} / {fmtBytes(s.bytes_in)}
                </td>
                <td className={cx(cell, "text-right")}>
                  {s.policy_hits > 0 && <Tag tone="warn">blocked {s.policy_hits}</Tag>}
                  {s.alerts > 0 && <Tag tone="fail">alerts {s.alerts}</Tag>}
                  {s.gaps > 0 && <Tag tone="muted">gaps</Tag>}
                  <Link to={`/audit/sessions/${s.session_id}`} className="ml-2 text-accent hover:underline">
                    ▶ replay
                  </Link>
                </td>
              </tr>
            );
          })}
          {items.length === 0 && (
            <tr>
              <td colSpan={showDevPod ? 9 : 8} className="px-4 py-8 text-center text-faint">
                No recorded sessions in this period.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}
