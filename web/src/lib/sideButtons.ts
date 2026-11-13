// A mouse's side buttons go back and forward. Chrome and Firefox do that
// themselves; Safari does nothing. So: when a side button is let go and
// the browser has not moved through its history a moment later, the page
// does it. Waiting (rather than always acting) keeps it to one step where
// the browser acts too.
const WAIT = 200; // ms: a browser's own step shows as popstate well within this

/** installSideButtons listens on win; returns what removes it. */
export function installSideButtons(win: Window = window): () => void {
  let moved = false;
  const onPop = () => {
    moved = true;
  };
  const onUp = (e: MouseEvent) => {
    if (e.button !== 3 && e.button !== 4) return;
    moved = false;
    const back = e.button === 3;
    win.setTimeout(() => {
      if (moved) return;
      if (back) win.history.back();
      else win.history.forward();
    }, WAIT);
  };
  win.addEventListener("popstate", onPop);
  win.addEventListener("mouseup", onUp, true); // capture: a terminal or a player that takes the mouse does not hide it
  return () => {
    win.removeEventListener("popstate", onPop);
    win.removeEventListener("mouseup", onUp, true);
  };
}
