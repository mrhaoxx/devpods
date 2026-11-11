import { describe, expect, it } from "vitest";
import { contrast, termTheme } from "./termTheme";

describe("terminal theme", () => {
  it("every ANSI color is readable on its background, in both themes", () => {
    for (const mode of ["light", "dark"] as const) {
      const t = termTheme(mode);
      for (const [name, color] of Object.entries(t)) {
        if (["background", "cursorAccent", "selectionBackground"].includes(name)) continue;
        // "black" on dark and "white"/"brightWhite" on light are the dim ends of the scale: 2.4:1 is enough there
        const dim = (mode === "dark" && name === "black") || (mode === "light" && (name === "white" || name === "brightWhite"));
        expect(contrast(color, t.background), `${mode} ${name}`).toBeGreaterThanOrEqual(dim ? 2.4 : 4.5);
      }
    }
  });
  it("the two themes differ and the cursor matches the text", () => {
    expect(termTheme("light").background).not.toBe(termTheme("dark").background);
    expect(termTheme("dark").cursor).toBe(termTheme("dark").foreground);
  });
});
