import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { YamlEditor } from "./YamlEditor";

function Host({ initial, check }: { initial: string; check?: boolean }) {
  const [v, setV] = useState(initial);
  return (
    <>
      <YamlEditor ariaLabel="YAML" value={v} onChange={setV} check={check} />
      <output data-testid="value">{v}</output>
    </>
  );
}
const settle = () => act(() => vi.advanceTimersByTime(300));

// (a test's DOM has no canvas and no workers: this is the editor without Monaco)
describe("YamlEditor", () => {
  afterEach(() => vi.useRealTimers());

  it("edits the text, and says nothing about text that is fine", () => {
    vi.useFakeTimers();
    render(<Host initial={"a: 1\n"} />);
    const box = screen.getByLabelText("YAML") as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "a: 1\nb: 2\n" } });
    settle();
    expect(screen.getByTestId("value").textContent).toBe("a: 1\nb: 2\n");
    expect(screen.queryByLabelText("YAML 语法")).toBeNull();
  });

  it("says where the text stops being YAML, a moment after the typing stops", () => {
    vi.useFakeTimers();
    render(<Host initial={"a: 1\n"} />);
    fireEvent.change(screen.getByLabelText("YAML"), { target: { value: "a: 1\nb:\n  c: [1, 2\n" } });
    expect(screen.queryByLabelText("YAML 语法")).toBeNull(); // not while typing
    settle();
    const note = screen.getByRole("alert", { name: "YAML 语法" });
    expect(note.textContent).toMatch(/YAML 语法错误/);
    expect(note.textContent).toMatch(/第 3 行第 \d+ 列/);
    // fixed: the note goes
    fireEvent.change(screen.getByLabelText("YAML"), { target: { value: "a: 1\nb:\n  c: [1, 2]\n" } });
    settle();
    expect(screen.queryByLabelText("YAML 语法")).toBeNull();
  });

  it("counts the problems when there are several", () => {
    vi.useFakeTimers();
    render(<Host initial={"a: [\nb: [\n"} />);
    settle();
    expect(screen.getByLabelText("YAML 语法").textContent).toMatch(/共 \d+ 处/);
  });

  it("check=false leaves the text alone", () => {
    vi.useFakeTimers();
    render(<Host initial={"a: [\n"} check={false} />);
    settle();
    expect(screen.queryByLabelText("YAML 语法")).toBeNull();
  });
});
