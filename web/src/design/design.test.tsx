import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { meterLevel } from "./Meter";
import { sparkPoints } from "./Sparkline";
import { StatusDot } from "./StatusDot";
import { Drawer } from "./Drawer";

describe("design primitives", () => {
  it("meterLevel thresholds", () => {
    expect(meterLevel(79, 100)).toBe("ok");
    expect(meterLevel(80, 100)).toBe("warn");
    expect(meterLevel(100, 100)).toBe("warn");
    expect(meterLevel(101, 100)).toBe("over");
    expect(meterLevel(5, 0)).toBe("ok");
  });

  it("sparkPoints scales to the box and splits at nulls", () => {
    const runs = sparkPoints([0, null, 10, 5], 30, 10, 10);
    expect(runs).toEqual(["0,10", "20,0 30,5"]);
  });

  it("StatusDot shows the phase name as text", () => {
    render(<StatusDot phase="Running" />);
    expect(screen.getByText("运行中")).toBeTruthy();
  });

  it("Drawer closes on Escape and calls onOpenChange(false)", () => {
    const onOpenChange = vi.fn();
    render(
      <Drawer open onOpenChange={onOpenChange} title="用户">
        <p>内容</p>
      </Drawer>,
    );
    expect(screen.getByText("内容")).toBeTruthy();
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
