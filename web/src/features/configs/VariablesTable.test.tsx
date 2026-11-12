import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { VariablesTable } from "./VariablesTable";

describe("VariablesTable", () => {
  it("a row per variable of the text; the built-in ones say what fills them", () => {
    render(<VariablesTable content={"a: ${GPU:-1}\nb: ${NAME}\nc: ${OWNER}"} decls={[]} onChange={() => {}} />);
    const list = within(screen.getByRole("list", { name: "变量" }));
    expect(list.getByText("GPU")).toBeTruthy();
    expect(list.getByText(/默认 1/)).toBeTruthy();
    expect(list.getByText(/自动填入当前用户/)).toBeTruthy();
    // only my own variables take a type
    expect(screen.getByLabelText("GPU 的类型")).toBeTruthy();
    expect(screen.queryByLabelText("NAME 的类型")).toBeNull();
  });

  it("a type and its options, a description: what is said of a variable", () => {
    const onChange = vi.fn();
    const { rerender } = render(<VariablesTable content={"a: ${GPU:-1}"} decls={[]} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("GPU 的类型"), { target: { value: "choice" } });
    expect(onChange).toHaveBeenLastCalledWith([{ name: "GPU", type: "choice" }]);
    rerender(<VariablesTable content={"a: ${GPU:-1}"} decls={[{ name: "GPU", type: "choice" }]} onChange={onChange} />);
    fireEvent.change(screen.getByLabelText("GPU 的选项"), { target: { value: "1, 2" } });
    expect(onChange).toHaveBeenLastCalledWith([{ name: "GPU", type: "choice", options: ["1", "2"] }]);
    fireEvent.change(screen.getByLabelText("GPU 的说明"), { target: { value: "每个的 GPU 数" } });
    expect(onChange).toHaveBeenLastCalledWith([{ name: "GPU", type: "choice", description: "每个的 GPU 数" }]);
  });

  it("a variable gone from the text takes what was said of it along", () => {
    const onChange = vi.fn();
    render(<VariablesTable content={"a: b"} decls={[{ name: "GPU", type: "int" }]} onChange={onChange} />);
    expect(onChange).toHaveBeenLastCalledWith([]);
    expect(screen.getByText("还没有变量")).toBeTruthy();
  });
});
