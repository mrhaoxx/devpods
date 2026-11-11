// Syntax check of YAML as it is typed: where the text stops being YAML,
// and why. (Whether it is a valid DevPod is the server's to say.)
import { LineCounter, parseAllDocuments } from "yaml";

export type YamlProblem = {
  severity: "error" | "warning";
  /** 1-based, as editors count */
  line: number;
  column: number;
  endLine: number;
  endColumn: number;
  message: string;
};

const MAX = 20;

/** the parser appends " at line 3, column 5:" and a frame of the source */
function oneLine(message: string): string {
  return message
    .split("\n")[0]
    .replace(/ at line \d+, column \d+:?$/, "")
    .trim();
}

export function yamlProblems(text: string): YamlProblem[] {
  if (text.trim() === "") return [];
  const lines = new LineCounter();
  const out: YamlProblem[] = [];
  let docs;
  try {
    docs = parseAllDocuments(text, { lineCounter: lines, prettyErrors: false, uniqueKeys: true });
  } catch (e) {
    return [{ severity: "error", line: 1, column: 1, endLine: 1, endColumn: 2, message: String(e) }];
  }
  // Something left open ("[1, 2") is noticed where the text ends, which
  // is the empty line after the last one: point at the end of what was
  // written instead.
  let end = text.length;
  while (end > 0 && /\s/.test(text[end - 1])) end--;
  const add = (severity: YamlProblem["severity"], e: { pos: [number, number]; message: string; code?: string }) => {
    if (out.length >= MAX) return;
    let [a, b] = e.pos;
    if (a >= end) [a, b] = [Math.max(0, end - 1), end];
    const from = lines.linePos(a);
    const to = lines.linePos(Math.max(b, a + 1));
    let message = oneLine(e.message);
    if (e.code === "DUPLICATE_KEY") {
      const key = text.slice(a).split("\n")[0].replace(/:(\s.*)?$/, "").trim().slice(0, 40);
      message = key ? `重复的键 ${key}（同一层里每个键只能出现一次）` : "重复的键（同一层里每个键只能出现一次）";
    }
    out.push({ severity, line: from.line, column: from.col, endLine: to.line, endColumn: to.col, message });
  };
  // `docs` is an EmptyStream (an array with nothing in it) for blank input
  for (const doc of docs as unknown as { errors: { pos: [number, number]; message: string; code?: string }[]; warnings: { pos: [number, number]; message: string; code?: string }[] }[]) {
    for (const e of doc.errors) add("error", e);
    for (const w of doc.warnings) add("warning", w);
  }
  return out.sort((a, b) => a.line - b.line || a.column - b.column);
}

/** what goes under the editor: the first problem, in words */
export function problemText(p: YamlProblem): string {
  return `第 ${p.line} 行第 ${p.column} 列：${p.message}`;
}
