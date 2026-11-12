// Monaco, trimmed to what the YAML editor needs: the editor core with its
// contributions (find, folding, hover, suggestions…), the YAML grammar,
// and monaco-yaml — a YAML language server in a worker — which checks a
// DevPod manifest against the DevPod schema (generated from the CRD by
// hack/webschema), completes its fields and shows what each one is.
// Loaded on demand — it is large — by YamlEditor.
import * as monaco from "monaco-editor/esm/vs/editor/editor.api";
import "monaco-editor/esm/vs/editor/edcore.main";
import "monaco-editor/esm/vs/basic-languages/yaml/yaml.contribution";
import EditorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker";
import { configureMonacoYaml, type JSONSchema } from "monaco-yaml";
import YamlWorker from "./yaml.worker?worker";
import devpodSchema from "./devpod.schema.json";

(self as unknown as { MonacoEnvironment: monaco.Environment }).MonacoEnvironment = {
  getWorker: (_id, label) => (label === "yaml" ? new YamlWorker() : new EditorWorker()),
};

/** What a model is: its URI says which schema it is checked against. */
export type YamlSchema = "devpod";
const schemaDir: Record<YamlSchema, string> = { devpod: "devpod" };

configureMonacoYaml(monaco, {
  enableSchemaRequest: false,
  hoverSchemaSource: false,
  completion: true,
  hover: true,
  validate: true,
  format: { enable: false }, // the text is the user's: never rewritten
  isKubernetes: true,
  // the schema is given here and never fetched (enableSchemaRequest is
  // off): the uri only names it. It must be absolute — a bare name is
  // taken for a path, looked for there, and not found.
  schemas: [{ uri: "https://devpod.haoxx.me/schemas/devpod.json", fileMatch: ["**/devpod/*.yaml"], schema: devpodSchema as JSONSchema }],
});

let models = 0;
const uriFor = (schema?: YamlSchema) => monaco.Uri.parse(`file:///${schema ? schemaDir[schema] : "plain"}/${++models}.yaml`);

/** monaco-yaml labels what the schema says with this; what the parser says with "YAML". */
const fromSchema = (m: monaco.editor.IMarker) => (m.source ?? "").startsWith("yaml-schema");

const css = (name: string, fallback: string) => {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  // Monaco takes #rrggbb only
  return /^#[0-9a-f]{6}([0-9a-f]{2})?$/i.test(v) ? v : fallback;
};

/** the console's colours under Monaco's own token colours */
export function applyTheme(): void {
  const dark = document.documentElement.dataset.theme === "dark";
  monaco.editor.defineTheme("devpod", {
    base: dark ? "vs-dark" : "vs",
    inherit: true,
    rules: [],
    colors: {
      "editor.background": css("--color-surface", dark ? "#16181d" : "#ffffff"),
      "editorGutter.background": css("--color-surface", dark ? "#16181d" : "#ffffff"),
      "editor.lineHighlightBackground": css("--color-sunk", dark ? "#1d2027" : "#f4f5f7"),
      "editorLineNumber.foreground": css("--color-faint", dark ? "#6b7280" : "#9ca3af"),
    },
  });
  monaco.editor.setTheme("devpod");
}

export type YamlEditorHandle = {
  setValue(text: string): void;
  setReadOnly(readOnly: boolean): void;
  dispose(): void;
};

/** What the schema says of the text: a field that does not exist, a value of the wrong type… */
export type SchemaProblem = { line: number; column: number; message: string; severity: "error" | "warning" };

export function createYamlEditor(
  el: HTMLElement,
  opts: {
    value: string;
    readOnly?: boolean;
    ariaLabel: string;
    schema?: YamlSchema;
    onChange(text: string): void;
    onSchemaProblems?(problems: SchemaProblem[]): void;
  },
): YamlEditorHandle {
  applyTheme();
  // (the language server underlines what is wrong — syntax and schema
  // alike — and YamlEditor says it below in words)
  const model = monaco.editor.createModel(opts.value, "yaml", uriFor(opts.schema));
  const editor = monaco.editor.create(el, {
    model,
    readOnly: opts.readOnly,
    ariaLabel: opts.ariaLabel,
    automaticLayout: true,
    minimap: { enabled: false },
    scrollBeyondLastLine: false,
    fontSize: 12,
    lineHeight: 19,
    fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--font-mono").trim() || "ui-monospace, monospace",
    tabSize: 2,
    insertSpaces: true, // YAML does not take tabs
    detectIndentation: false,
    renderWhitespace: "boundary",
    wordWrap: "off",
    folding: true,
    lineNumbersMinChars: 3,
    padding: { top: 6, bottom: 6 },
    scrollbar: { alwaysConsumeMouseWheel: false },
    overviewRulerLanes: 0,
    fixedOverflowWidgets: true,
    // fields are offered as they are typed (YAML values count as strings
    // to Monaco), from the schema — not every word already in the text
    quickSuggestions: { other: true, strings: true, comments: false },
    quickSuggestionsDelay: 50,
    suggestOnTriggerCharacters: true,
    wordBasedSuggestions: "off",
    suggest: { showWords: false, snippetsPreventQuickSuggestions: false },
  });
  let silent = false;
  // (as tall as its box: YamlEditor sizes the box to the text)
  const sub = model.onDidChangeContent(() => {
    if (!silent) opts.onChange(model.getValue());
  });
  const markers = monaco.editor.onDidChangeMarkers((uris) => {
    if (!opts.onSchemaProblems || !uris.some((u) => u.toString() === model.uri.toString())) return;
    opts.onSchemaProblems(
      monaco.editor
        .getModelMarkers({ resource: model.uri })
        .filter(fromSchema)
        .sort((a, b) => a.startLineNumber - b.startLineNumber || a.startColumn - b.startColumn)
        .map((m) => ({
          line: m.startLineNumber,
          column: m.startColumn,
          message: m.message,
          severity: m.severity === monaco.MarkerSeverity.Error ? "error" : "warning",
        })),
    );
  });
  // the page's theme switch flips data-theme on <html>
  const themeWatch = new MutationObserver(applyTheme);
  themeWatch.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  return {
    setValue(text) {
      if (text === model.getValue()) return;
      silent = true;
      // keeps undo history and the cursor where it can
      editor.executeEdits("external", [{ range: model.getFullModelRange(), text }]);
      editor.pushUndoStop();
      silent = false;
    },
    setReadOnly(readOnly) {
      editor.updateOptions({ readOnly });
    },
    dispose() {
      themeWatch.disconnect();
      markers.dispose();
      sub.dispose();
      editor.dispose();
      model.dispose();
    },
  };
}
