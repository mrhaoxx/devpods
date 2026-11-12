// A config's YAML has variables where values go (${GPU:-1}): until they
// are filled, what the schema says of such a value — "not an integer",
// "not one of bash, zsh, fish" — is not true of the DevPod it becomes.
// Those complaints are dropped; the rest (a misspelt field…) stand.

export type MarkerLike = { startLineNumber: number; startColumn: number; endLineNumber: number; endColumn: number; message: string };

/** the text a marker covers */
function covered(m: MarkerLike, lineText: (line: number) => string): string {
  const parts: string[] = [];
  for (let l = m.startLineNumber; l <= m.endLineNumber; l++) {
    const t = lineText(l) ?? "";
    const from = l === m.startLineNumber ? m.startColumn - 1 : 0;
    const to = l === m.endLineNumber ? m.endColumn - 1 : t.length;
    parts.push(t.slice(from, to));
  }
  return parts.join("\n");
}

/** templatedMarkers: the markers that are not about a ${...}. */
export function templatedMarkers<T extends MarkerLike>(markers: T[], lineText: (line: number) => string): T[] {
  return markers.filter((m) => !covered(m, lineText).includes("${"));
}
