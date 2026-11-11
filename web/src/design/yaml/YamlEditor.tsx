// A YAML editor: Monaco with highlighting where a browser can run it, a
// plain text box otherwise (and until Monaco has loaded). Either way the
// text is checked as it is typed and what is wrong with it is said below:
// in Monaco the place is underlined, too.
import { useEffect, useMemo, useRef, useState } from "react";
import { Textarea } from "../Form";
import { problemText, yamlProblems, type YamlProblem } from "./lint";
import type { YamlEditorHandle } from "./monaco";

/** Monaco draws on a canvas and parses in a worker: not in a test's DOM. */
function canRunMonaco(): boolean {
  if (typeof window === "undefined" || typeof Worker === "undefined") return false;
  try {
    return !!document.createElement("canvas").getContext("2d");
  } catch {
    return false;
  }
}

export function YamlEditor({
  value,
  onChange,
  ariaLabel,
  readOnly = false,
  minLines = 14,
  maxLines = 40,
  check = true,
}: {
  value: string;
  onChange?: (text: string) => void;
  ariaLabel: string;
  readOnly?: boolean;
  minLines?: number;
  maxLines?: number;
  /** false: no syntax check (text that is not YAML yet) */
  check?: boolean;
}) {
  const host = useRef<HTMLDivElement>(null);
  const handle = useRef<YamlEditorHandle | null>(null);
  const [rich, setRich] = useState<"loading" | "on" | "off">(() => (canRunMonaco() ? "loading" : "off"));
  const latest = useRef({ value, onChange });
  latest.current = { value, onChange };

  // checked a moment after the typing stops
  const [checked, setChecked] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setChecked(value), 250);
    return () => clearTimeout(t);
  }, [value]);
  const problems: YamlProblem[] = useMemo(() => (check ? yamlProblems(checked) : []), [check, checked]);

  useEffect(() => {
    if (rich === "off" || !host.current) return;
    let gone = false;
    const el = host.current;
    import("./monaco")
      .then((m) => {
        if (gone) return;
        handle.current = m.createYamlEditor(el, {
          value: latest.current.value,
          readOnly,
          ariaLabel,
          minLines,
          maxLines,
          onChange: (text) => latest.current.onChange?.(text),
        });
        setRich("on");
      })
      .catch(() => !gone && setRich("off")); // the chunk did not load: the text box still works
    return () => {
      gone = true;
      handle.current?.dispose();
      handle.current = null;
    };
    // created once per mount; later changes go through the handle below
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rich === "off"]);

  useEffect(() => handle.current?.setValue(value), [value, rich]);
  useEffect(() => handle.current?.setReadOnly(readOnly), [readOnly, rich]);
  useEffect(() => handle.current?.setProblems(problems), [problems, rich]);

  const lines = Math.min(maxLines, Math.max(minLines, value.split("\n").length + 1));
  const errors = problems.filter((p) => p.severity === "error");
  const first = errors[0] ?? problems[0];
  return (
    <div className="space-y-1.5">
      {rich !== "off" && <div ref={host} data-testid="yaml-monaco" className={`overflow-hidden rounded-md border border-line ${rich === "loading" ? "hidden" : ""}`} />}
      {rich !== "on" && (
        <Textarea
          aria-label={ariaLabel}
          rows={lines}
          value={value}
          readOnly={readOnly}
          onChange={(e) => onChange?.(e.target.value)}
          className="mono w-full text-xs leading-5"
          spellCheck={false}
          wrap="off"
        />
      )}
      {first && (
        <p role={errors.length > 0 ? "alert" : "status"} aria-label="YAML 语法" className={`rounded-md px-3 py-1.5 text-xs ${errors.length > 0 ? "bg-fail-soft text-fail" : "bg-warm-soft text-warm"}`}>
          <span className="font-medium">{errors.length > 0 ? "YAML 语法错误" : "YAML 提示"}</span>
          {" · "}
          <span className="mono">{problemText(first)}</span>
          {problems.length > 1 && <span className="text-muted">（共 {problems.length} 处）</span>}
        </p>
      )}
    </div>
  );
}
