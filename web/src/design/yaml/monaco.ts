// Monaco, trimmed to what the YAML editor needs: the editor core with its
// contributions (find, folding, hover on a problem…) and the YAML grammar.
// Loaded on demand — it is large — by YamlEditor.
import * as monaco from "monaco-editor/esm/vs/editor/editor.api";
import "monaco-editor/esm/vs/editor/edcore.main";
import "monaco-editor/esm/vs/basic-languages/yaml/yaml.contribution";
import EditorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker";
import type { YamlProblem } from "./lint";

(self as unknown as { MonacoEnvironment: monaco.Environment }).MonacoEnvironment = { getWorker: () => new EditorWorker() };

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
  setProblems(problems: YamlProblem[]): void;
  setReadOnly(readOnly: boolean): void;
  dispose(): void;
};

export function createYamlEditor(el: HTMLElement, opts: { value: string; readOnly?: boolean; ariaLabel: string; minLines: number; maxLines: number; onChange(text: string): void }): YamlEditorHandle {
  applyTheme();
  const model = monaco.editor.createModel(opts.value, "yaml");
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
  });
  // as tall as its text, within bounds
  const fit = () => {
    const lines = Math.min(opts.maxLines, Math.max(opts.minLines, model.getLineCount() + 1));
    el.style.height = `${lines * 19 + 12}px`;
  };
  fit();
  let silent = false;
  const sub = model.onDidChangeContent(() => {
    fit();
    if (!silent) opts.onChange(model.getValue());
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
    setProblems(problems) {
      monaco.editor.setModelMarkers(
        model,
        "yaml-syntax",
        problems.map((p) => ({
          severity: p.severity === "error" ? monaco.MarkerSeverity.Error : monaco.MarkerSeverity.Warning,
          startLineNumber: p.line,
          startColumn: p.column,
          endLineNumber: p.endLine,
          endColumn: p.endColumn,
          message: p.message,
          source: "YAML",
        })),
      );
    },
    setReadOnly(readOnly) {
      editor.updateOptions({ readOnly });
    },
    dispose() {
      themeWatch.disconnect();
      sub.dispose();
      editor.dispose();
      model.dispose();
    },
  };
}
