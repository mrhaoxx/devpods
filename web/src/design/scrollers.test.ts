// Anything that scrolls is positioned (relative, absolute, fixed or sticky):
// what its rows position — a status dot's sr-only label is absolute — then
// stays inside it, clipped and scrolled with it. Unpositioned, such an
// element belongs to the page's <main> instead, at its row's depth in the
// unscrolled list: 400 nodes made the page as tall as the whole table.
import { describe, expect, it } from "vitest";

const sources = import.meta.glob(["/src/**/*.tsx", "!/src/**/*.test.tsx"], { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const scrolls = /\boverflow-(?:[xy]-)?(?:auto|scroll)\b/;
const positioned = /(?:^|[\s:])(?:relative|absolute|fixed|sticky)(?:$|\s)/;

describe("scrollers", () => {
  it("every class string that scrolls also positions", () => {
    const bad: string[] = [];
    for (const [file, text] of Object.entries(sources)) {
      text.split("\n").forEach((line, i) => {
        for (const m of line.matchAll(/"([^"]*)"|`([^`]*)`|'([^']*)'/g)) {
          const cls = m[1] ?? m[2] ?? m[3] ?? "";
          if (scrolls.test(cls) && !positioned.test(cls)) bad.push(`${file}:${i + 1}: ${cls.trim()}`);
        }
      });
    }
    expect(bad).toEqual([]);
  });
});
