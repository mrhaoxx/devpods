// Two YAML texts side by side — what a DevPod is and what it would
// become — in Monaco's diff view where it runs, as two blocks otherwise.
import { useEffect, useRef, useState } from "react";
import { canRunMonaco, preloadYamlEditor } from "./YamlEditor";

export function YamlDiff({ original, modified, height = 360 }: { original: string; modified: string; height?: number }) {
  const host = useRef<HTMLDivElement>(null);
  const [rich, setRich] = useState(() => canRunMonaco());
  useEffect(() => {
    if (!rich || !host.current) return;
    let gone = false;
    let view: { dispose(): void } | null = null;
    const el = host.current;
    preloadYamlEditor()
      ?.then((m) => {
        if (!gone) view = m.createYamlDiff(el, original, modified);
      })
      .catch(() => !gone && setRich(false));
    return () => {
      gone = true;
      view?.dispose();
    };
  }, [rich, original, modified]);
  if (rich) return <div ref={host} data-testid="yaml-diff" className="overflow-hidden rounded-md border border-line" style={{ height }} />;
  return (
    <div data-testid="yaml-diff" className="grid grid-cols-2 gap-2">
      {[original, modified].map((t, i) => (
        <pre key={i} aria-label={i === 0 ? "现在" : "按新配置"} className="mono max-h-96 overflow-auto rounded-md border border-line bg-sunk p-2 text-[11px] whitespace-pre">
          {t}
        </pre>
      ))}
    </div>
  );
}
