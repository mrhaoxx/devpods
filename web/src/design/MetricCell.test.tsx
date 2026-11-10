import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MetricCell } from "./MetricCell";

describe("MetricCell", () => {
  it("shows — with the rebuild hint for null", () => {
    render(<MetricCell value={null} text="7.4/8" />);
    const dash = screen.getByLabelText("监控在 Pod 下次重建后生效");
    expect(dash.textContent).toBe("—");
    expect(screen.queryByText("7.4/8")).toBeNull();
  });
  it("shows the text when there is a value", () => {
    render(<MetricCell value={7.4} text="7.4/8" spark={[1, 2, 3]} />);
    expect(screen.getByText("7.4/8")).toBeTruthy();
  });
});
