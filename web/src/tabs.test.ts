import { describe, expect, it } from "vitest";
import { tabFromHash } from "./tabs";

describe("tabFromHash", () => {
  const tabs = ["overview", "sessions", "events"];
  it("selects the tab named in the hash", () => {
    expect(tabFromHash("#sessions", tabs)).toBe("sessions");
    expect(tabFromHash("events", tabs)).toBe("events");
  });
  it("falls back to the first tab", () => {
    expect(tabFromHash("", tabs)).toBe("overview");
    expect(tabFromHash("#bogus", tabs)).toBe("overview");
  });
});
