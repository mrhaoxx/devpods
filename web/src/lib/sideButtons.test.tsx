import { afterEach, expect, it, vi } from "vitest";
import { installSideButtons } from "./sideButtons";

afterEach(() => vi.useRealTimers());

it("a side button steps through history when the browser does not, and only then", () => {
  vi.useFakeTimers();
  const back = vi.spyOn(window.history, "back").mockImplementation(() => {});
  const forward = vi.spyOn(window.history, "forward").mockImplementation(() => {});
  const remove = installSideButtons();
  const up = (button: number) => window.dispatchEvent(new MouseEvent("mouseup", { button }));

  up(3); // Safari: nothing happens by itself
  vi.advanceTimersByTime(200);
  expect(back).toHaveBeenCalledTimes(1);
  up(4);
  vi.advanceTimersByTime(200);
  expect(forward).toHaveBeenCalledTimes(1);

  up(3); // Chrome: the browser went back itself
  window.dispatchEvent(new PopStateEvent("popstate"));
  vi.advanceTimersByTime(200);
  expect(back).toHaveBeenCalledTimes(1);

  up(0); // other buttons are not ours
  vi.advanceTimersByTime(200);
  expect(back).toHaveBeenCalledTimes(1);
  remove();
});
