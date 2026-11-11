import { describe, expect, it } from "vitest";
import { termClipboardAction, termKeySequence } from "./termKeys";

const k = (key: string, mods: Partial<{ altKey: boolean; metaKey: boolean; ctrlKey: boolean; shiftKey: boolean }> = {}) => ({ key, altKey: false, metaKey: false, ctrlKey: false, shiftKey: false, ...mods });

describe("terminal shortcuts", () => {
  it("Option / Alt moves and deletes by word", () => {
    expect(termKeySequence(k("ArrowRight", { altKey: true }), true)).toBe("\x1bf");
    expect(termKeySequence(k("ArrowLeft", { altKey: true }), false)).toBe("\x1bb");
    expect(termKeySequence(k("Backspace", { altKey: true }), true)).toBe("\x1b\x7f");
    expect(termKeySequence(k("Delete", { altKey: true }), true)).toBe("\x1bd");
  });
  it("⌘ moves and deletes by line, on macOS only", () => {
    expect(termKeySequence(k("ArrowLeft", { metaKey: true }), true)).toBe("\x01");
    expect(termKeySequence(k("ArrowRight", { metaKey: true }), true)).toBe("\x05");
    expect(termKeySequence(k("Backspace", { metaKey: true }), true)).toBe("\x15");
    expect(termKeySequence(k("ArrowLeft", { metaKey: true }), false)).toBeNull();
  });
  it("leaves everything else to the terminal", () => {
    expect(termKeySequence(k("ArrowRight"), true)).toBeNull();
    expect(termKeySequence(k("ArrowRight", { ctrlKey: true }), true)).toBeNull();
    expect(termKeySequence(k("ArrowRight", { altKey: true, shiftKey: true }), true)).toBeNull();
    expect(termKeySequence(k("f", { altKey: true }), true)).toBeNull();
  });
  it("Ctrl+Shift+C / V copy and paste off macOS", () => {
    expect(termClipboardAction(k("C", { ctrlKey: true, shiftKey: true }), false)).toBe("copy");
    expect(termClipboardAction(k("v", { ctrlKey: true, shiftKey: true }), false)).toBe("paste");
    expect(termClipboardAction(k("c", { ctrlKey: true }), false)).toBeNull();
    expect(termClipboardAction(k("C", { ctrlKey: true, shiftKey: true }), true)).toBeNull();
  });
});
