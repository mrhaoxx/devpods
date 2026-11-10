import { useState } from "react";

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // http origins have no clipboard API: fall back to a selection copy
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand?.("copy") ?? false;
    ta.remove();
    return ok;
  }
}

function useCopied(): [boolean, (v: string) => void] {
  const [done, setDone] = useState(false);
  return [
    done,
    async (v) => {
      if (await copyText(v)) {
        setDone(true);
        setTimeout(() => setDone(false), 1200);
      }
    },
  ];
}

/** One-line copyable value (an SSH command). */
export function CopyField({ value, label = "复制" }: { value: string; label?: string }) {
  const [done, copy] = useCopied();
  return (
    <div className="flex min-w-0 items-center gap-2 rounded-md border border-line bg-sunk px-2 py-1">
      <code className="mono min-w-0 flex-1 truncate text-xs" title={value}>
        {value}
      </code>
      <button type="button" onClick={() => copy(value)} className="shrink-0 text-[11px] text-muted hover:text-accent">
        {done ? "已复制" : label}
      </button>
    </div>
  );
}

/** Multi-line copyable block (an ssh config snippet). */
export function CopyBlock({ value }: { value: string }) {
  const [done, copy] = useCopied();
  return (
    <div className="relative rounded-md border border-line bg-sunk">
      <button
        type="button"
        onClick={() => copy(value)}
        className="absolute right-2 top-2 rounded border border-line bg-surface px-1.5 py-0.5 text-[11px] text-muted hover:text-accent"
      >
        {done ? "已复制" : "复制"}
      </button>
      <pre className="mono overflow-x-auto p-3 pr-14 text-xs leading-5">{value}</pre>
    </div>
  );
}
