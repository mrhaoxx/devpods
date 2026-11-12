// A YAML editor: Monaco where a browser can run it, a plain text box
// otherwise. Monaco is large and arrives a moment after the page: until
// then its box already stands where it will be, as tall as it will be,
// with the text in it — nothing is swapped in front of the reader.
//
// The text is checked as it is typed and what is wrong is said below it.
// With a schema ("devpod"), Monaco also completes the fields, shows what
// each one is on hover, and points out a field that does not exist or a
// value of the wrong type.
import { useEffect, useMemo, useRef, useState } from "react";
import { Textarea } from "../Form";
import { problemText, yamlProblems, type YamlProblem } from "./lint";
import type { SchemaProblem, YamlEditorHandle, YamlSchema } from "./monaco";

/** Monaco draws on a canvas and parses in a worker: not in a test's DOM. */
export function canRunMonaco(): boolean {
  if (typeof window === "undefined" || typeof Worker === "undefined") return false;
  try {
    return !!document.createElement("canvas").getContext("2d");
  } catch {
    return false;
  }
}

let loading: Promise<typeof import("./monaco")> | null = null;
/** preloadYamlEditor starts fetching Monaco before an editor is shown. */
export function preloadYamlEditor(): Promise<typeof import("./monaco")> | null {
  if (!canRunMonaco()) return null;
  loading ??= import("./monaco").catch((e) => {
    loading = null; // a later editor tries again
    throw e;
  });
  return loading;
}

const LINE = 19; // px: Monaco's line height, and the placeholder's
const PAD = 6;

export function YamlEditor({
  value,
  onChange,
  ariaLabel,
  readOnly = false,
  minLines = 14,
  maxLines = 40,
  check = true,
  schema,
  templated = false,
}: {
  value: string;
  onChange?: (text: string) => void;
  ariaLabel: string;
  readOnly?: boolean;
  minLines?: number;
  maxLines?: number;
  /** false: no syntax check (text that is not YAML yet) */
  check?: boolean;
  /** what the text is: fields are completed, explained and checked against it */
  schema?: YamlSchema;
  /** the text has ${...} where values go (a config): nothing is said of those */
  templated?: boolean;
}) {
  const host = useRef<HTMLDivElement>(null);
  const handle = useRef<YamlEditorHandle | null>(null);
  const [rich, setRich] = useState<"loading" | "on" | "off">(() => (canRunMonaco() ? "loading" : "off"));
  const latest = useRef({ value, onChange });
  latest.current = { value, onChange };
  const [schemaProblems, setSchemaProblems] = useState<SchemaProblem[]>([]);

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
    const load = preloadYamlEditor();
    if (!load) return;
    load
      .then((m) => {
        if (gone) return;
        handle.current = m.createYamlEditor(el, {
          value: latest.current.value,
          readOnly,
          ariaLabel,
          schema,
          templated,
          onChange: (text) => latest.current.onChange?.(text),
          onSchemaProblems: (ps) => !gone && setSchemaProblems(ps),
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

  const lineCount = value.split("\n").length;
  const lines = Math.min(maxLines, Math.max(minLines, lineCount + 1));
  const errors = problems.filter((p) => p.severity === "error");
  const first = errors[0] ?? problems[0];
  // what the schema says is told when the text itself is fine
  const field = !first && rich === "on" ? schemaProblems[0] : undefined;
  return (
    <div className="space-y-1.5">
      {rich !== "off" && (
        <div
          data-testid="yaml-monaco"
          className="relative overflow-hidden rounded-md border border-line bg-surface"
          style={{ height: lines * LINE + 2 * PAD }}
        >
          <div ref={host} className="absolute inset-0" />
          {rich === "loading" && (
            // the text where Monaco will show it, until it does
            <pre aria-hidden="true" className="mono absolute inset-0 m-0 overflow-hidden text-xs" style={{ paddingTop: PAD, lineHeight: `${LINE}px` }}>
              {value.split("\n").map((l, i) => (
                <div key={i} className="flex whitespace-pre">
                  <span className="w-[42px] shrink-0 pr-[18px] text-right text-faint">{i + 1}</span>
                  <span className="text-ink">{l}</span>
                </div>
              ))}
            </pre>
          )}
        </div>
      )}
      {rich === "off" && (
        <Textarea
          data-testid="yaml-plain"
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
      {field && (
        <p role="status" aria-label="YAML 字段" className="rounded-md bg-warm-soft px-3 py-1.5 text-xs text-warm">
          <span className="font-medium">字段提示</span>
          {" · "}
          <span className="mono">{problemText({ ...field, endLine: field.line, endColumn: field.column })}</span>
          {schemaProblems.length > 1 && <span className="text-muted">（共 {schemaProblems.length} 处）</span>}
        </p>
      )}
    </div>
  );
}
