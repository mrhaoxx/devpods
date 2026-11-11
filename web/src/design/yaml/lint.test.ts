import { describe, expect, it } from "vitest";
import { yamlProblems } from "./lint";

describe("yamlProblems", () => {
  it("finds nothing wrong with a DevPod manifest", () => {
    const text = "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: a-b\nspec:\n  running: true\n  pod:\n    spec:\n      containers:\n        - name: dev\n          image: ubuntu:24.04\n          command: [sleep, infinity]\n";
    expect(yamlProblems(text)).toEqual([]);
    expect(yamlProblems("")).toEqual([]);
  });

  it("points at where the syntax breaks: 1-based line and column, with an end", () => {
    const p = yamlProblems("spec:\n  running: true\n  pod: [unclosed\n");
    expect(p.length).toBeGreaterThan(0);
    expect(p[0].severity).toBe("error");
    expect(p[0].line).toBe(3);
    expect(p[0].column).toBeGreaterThan(1);
    expect(p[0].endLine).toBeGreaterThanOrEqual(p[0].line);
    expect(p[0].message).not.toMatch(/\n/); // one line: no code frame of the parser's
    expect(p[0].message.length).toBeGreaterThan(5);
  });

  it("bad indentation and a tab used to indent are errors", () => {
    expect(yamlProblems("a:\n  b: 1\n c: 2\n")[0]).toMatchObject({ severity: "error", line: 3 });
    expect(yamlProblems("a:\n\tb: 1\n")[0]).toMatchObject({ severity: "error", line: 2 });
  });

  it("a key written twice is an error on its second line", () => {
    const p = yamlProblems("spec:\n  running: true\n  running: false\n");
    expect(p[0]).toMatchObject({ severity: "error", line: 3 });
    expect(p[0].message).toMatch(/running/);
  });

  it("every document of a stream is checked, with lines of the whole text", () => {
    const p = yamlProblems("a: 1\n---\nb: [1, 2\n");
    expect(p[0]).toMatchObject({ severity: "error", line: 3 });
  });

  it("placeholders of a config are text like any other", () => {
    expect(yamlProblems("metadata:\n  name: ${NAME}\nspec:\n  owner: ${OWNER}\n  image: ${IMAGE:-ubuntu:24.04}\n  gpu: '${GPU}'\n")).toEqual([]);
  });

  it("reports at most a handful", () => {
    const many = Array.from({ length: 60 }, (_, i) => `k${i}: [`).join("\n");
    expect(yamlProblems(many).length).toBeLessThanOrEqual(20);
  });
});
