// Shared setup for jsdom component tests (vite.config.ts: test.setupFiles).
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

afterEach(() => cleanup());

if (typeof window !== "undefined") {
  // jsdom has no matchMedia: report "no match" (light scheme, wide screen).
  if (!window.matchMedia) {
    window.matchMedia = (query: string) =>
      ({
        matches: false,
        media: query,
        onchange: null,
        addEventListener: () => {},
        removeEventListener: () => {},
        addListener: () => {},
        removeListener: () => {},
        dispatchEvent: () => false,
      }) as MediaQueryList;
  }
  if (!("EventSource" in window)) {
    // streams stay silent in component tests unless a test drives them
    class ES {
      onopen = null;
      onmessage = null;
      onerror = null;
      constructor(public url: string) {}
      addEventListener() {}
      close() {}
    }
    (window as unknown as { EventSource: unknown }).EventSource = ES;
    (globalThis as unknown as { EventSource: unknown }).EventSource = ES;
  }
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {};
  if (!("ResizeObserver" in window)) {
    class RO {
      observe() {}
      unobserve() {}
      disconnect() {}
    }
    (window as unknown as { ResizeObserver: unknown }).ResizeObserver = RO;
  }
}
