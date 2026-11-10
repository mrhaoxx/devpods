import { describe, expect, it } from "vitest";
import { readPref, resolveTheme, writePref, THEME_KEY } from "./theme";

const throwing = {
  getItem: () => {
    throw new Error("SecurityError");
  },
  setItem: () => {
    throw new Error("QuotaExceeded");
  },
};

describe("theme preference", () => {
  it("readPref returns system when storage throws", () => {
    expect(readPref(throwing)).toBe("system");
    expect(readPref(undefined)).toBe("system");
  });
  it("readPref ignores unknown values", () => {
    expect(readPref({ getItem: () => "purple" })).toBe("system");
    expect(readPref({ getItem: (k: string) => (k === THEME_KEY ? "dark" : null) })).toBe("dark");
  });
  it("resolveTheme follows prefersDark only for system", () => {
    expect(resolveTheme("system", true)).toBe("dark");
    expect(resolveTheme("system", false)).toBe("light");
    expect(resolveTheme("light", true)).toBe("light");
    expect(resolveTheme("dark", false)).toBe("dark");
  });
  it("writePref swallows storage errors", () => {
    expect(() => writePref(throwing, "dark")).not.toThrow();
    const saved: Record<string, string> = {};
    writePref({ setItem: (k: string, v: string) => (saved[k] = v) }, "light");
    expect(saved[THEME_KEY]).toBe("light");
  });
});
