// Editing shortcuts for the web terminal, the way desktop terminals map
// them ("natural text editing"): the page sends the sequence itself and
// swallows the key, so neither xterm's Option-key composition nor the
// browser (Alt+← is "back" on Windows / Linux, ⌘+← on macOS) gets it.
export type KeyLike = { key: string; altKey: boolean; metaKey: boolean; ctrlKey: boolean; shiftKey: boolean };

const ESC = "\x1b";

/** termKeySequence: the bytes to send for an editing shortcut, or null
 *  when the key is not one (xterm then handles it as usual). */
export function termKeySequence(e: KeyLike, mac: boolean): string | null {
  const only = (alt: boolean, meta: boolean, ctrl: boolean) => e.altKey === alt && e.metaKey === meta && e.ctrlKey === ctrl && !e.shiftKey;
  // Option / Alt: by word
  if (only(true, false, false)) {
    switch (e.key) {
      case "ArrowLeft":
        return ESC + "b"; // back one word
      case "ArrowRight":
        return ESC + "f"; // forward one word
      case "Backspace":
        return ESC + "\x7f"; // delete the word before the cursor
      case "Delete":
        return ESC + "d"; // delete the word after the cursor
    }
  }
  // ⌘ on macOS: by line
  if (mac && only(false, true, false)) {
    switch (e.key) {
      case "ArrowLeft":
        return "\x01"; // start of line (Ctrl-A)
      case "ArrowRight":
        return "\x05"; // end of line (Ctrl-E)
      case "Backspace":
        return "\x15"; // delete to the start of the line (Ctrl-U)
      case "Delete":
        return "\x0b"; // delete to the end of the line (Ctrl-K)
    }
  }
  return null;
}

/** Ctrl+Shift+C / V copy and paste where Ctrl+C / V belong to the shell
 *  (Windows, Linux); macOS uses ⌘C / ⌘V natively. */
export function termClipboardAction(e: KeyLike, mac: boolean): "copy" | "paste" | null {
  if (mac || !e.ctrlKey || !e.shiftKey || e.altKey || e.metaKey) return null;
  const k = e.key.toLowerCase();
  return k === "c" ? "copy" : k === "v" ? "paste" : null;
}
