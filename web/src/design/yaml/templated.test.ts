import { describe, expect, it } from "vitest";
import { templatedMarkers, type MarkerLike } from "./templated";

const text = ["spec:", "  runnning: true", "    nvidia.com/gpu: ${GPU:-1}", "  shell: ${SHELL}"];
const line = (n: number) => text[n - 1];
const at = (l: number, from: number, to: number, message: string): MarkerLike => ({ startLineNumber: l, startColumn: from, endLineNumber: l, endColumn: to, message });

describe("templatedMarkers", () => {
  it("drops what is said of a ${...} (its value is not known yet), keeps the rest", () => {
    const gpu = at(3, 21, 30, 'Incorrect type. Expected "integer | string".');
    const shell = at(4, 10, 18, 'Value is not accepted. Valid values: "bash", "zsh", "fish".');
    const misspelt = at(2, 3, 11, "Property runnning is not allowed.");
    expect(templatedMarkers([gpu, misspelt, shell], line)).toEqual([misspelt]);
  });

  it("keeps a type complaint about plain text", () => {
    const plain = at(2, 13, 17, "Incorrect type. Expected \"object\".");
    expect(templatedMarkers([plain], line)).toEqual([plain]);
  });
});
