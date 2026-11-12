import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { useState } from "react";
import { DataTable, columnsForWidth } from "./DataTable";

type Row = { name: string; cpu: number };
const rows: Row[] = [
  { name: "a", cpu: 1 },
  { name: "b", cpu: 3 },
  { name: "c", cpu: 2 },
];
const columns: ColumnDef<Row, unknown>[] = [
  { id: "name", header: "名称", accessorKey: "name", meta: { priority: 1 } },
  { id: "cpu", header: "CPU", accessorKey: "cpu", meta: { priority: 2 } },
];

function Harness({ onSort = () => {} }: { onSort?: (s: SortingState) => void }) {
  const [sorting, setSorting] = useState<SortingState>([]);
  return (
    <DataTable
      data={rows}
      columns={columns}
      getRowId={(r) => r.name}
      sorting={sorting}
      onSortingChange={(s) => {
        setSorting(s);
        onSort(s);
      }}
      selectable
      bulkBar={(sel) => <button>休眠 {sel.map((r) => r.name).join(",")}</button>}
    />
  );
}

describe("DataTable", () => {
  it("selecting two rows shows 已选 2 and the bulk bar", () => {
    render(<Harness />);
    fireEvent.click(screen.getByLabelText("选择 a"));
    fireEvent.click(screen.getByLabelText("选择 c"));
    expect(screen.getByText("已选 2")).toBeTruthy();
    expect(screen.getByRole("button", { name: "休眠 a,c" })).toBeTruthy();
  });

  it("header checkbox selects all shown rows", () => {
    render(<Harness />);
    fireEvent.click(screen.getByLabelText("全选"));
    expect(screen.getByText("已选 3")).toBeTruthy();
  });

  it("clear hides the bulk bar", () => {
    render(<Harness />);
    fireEvent.click(screen.getByLabelText("选择 b"));
    fireEvent.click(screen.getByRole("button", { name: "取消选择" }));
    expect(screen.queryByText(/已选/)).toBeNull();
  });

  it("clicking a sortable header calls onSortingChange", () => {
    const onSort = vi.fn();
    render(<Harness onSort={onSort} />);
    fireEvent.click(screen.getByRole("button", { name: /CPU/ }));
    expect(onSort).toHaveBeenCalledWith([{ id: "cpu", desc: true }]);
    const cells = screen.getAllByRole("row").slice(1).map((r) => r.textContent);
    expect(cells[0]).toContain("b");
  });

  it("columnsForWidth drops the highest priority numbers first", () => {
    const metas = [1, 2, 3, 4, 5].map((p) => ({ id: `c${p}`, priority: p, width: 100 }));
    expect(columnsForWidth(250, metas)).toEqual(["c1", "c2"]);
    expect(columnsForWidth(0, metas)).toHaveLength(5); // width unknown: show all
    expect(columnsForWidth(10, metas)).toEqual(["c1"]); // priority 1 always shows
  });

  describe("column widths", () => {
    afterEach(() => localStorage.clear());
    const sized: ColumnDef<Row, unknown>[] = [
      { id: "name", header: "名称", accessorKey: "name", meta: { priority: 1, width: 150 } },
      { id: "cpu", header: "CPU", accessorKey: "cpu", meta: { priority: 1, width: 90 } },
    ];
    const table = () => render(<DataTable data={rows} columns={sized} getRowId={(r) => r.name} sorting={[]} onSortingChange={() => {}} storageKey="t" />);
    const colWidth = (name: string) => (screen.getByRole("columnheader", { name: new RegExp(name) }) as HTMLElement).style.width;

    it("a column is as wide as it says, whatever its cells hold", () => {
      table();
      expect(screen.getByRole("table").style.tableLayout).toBe("fixed");
      expect(colWidth("名称")).toBe("150px");
      expect(colWidth("CPU")).toBe("90px");
    });

    it("a width once set is kept", () => {
      localStorage.setItem("devpod.columns.t", JSON.stringify({ name: 260 }));
      table();
      expect(colWidth("名称")).toBe("260px");
      expect(colWidth("CPU")).toBe("90px");
    });

    it("dragging a column's edge sets its width, and it is remembered", () => {
      table();
      const edge = screen.getByRole("separator", { name: "调整 名称 列宽" });
      fireEvent.mouseDown(edge, { clientX: 100 });
      fireEvent.mouseMove(document, { clientX: 140 });
      fireEvent.mouseUp(document, { clientX: 140 });
      expect(colWidth("名称")).toBe("190px");
      expect(JSON.parse(localStorage.getItem("devpod.columns.t") ?? "{}").name).toBe(190);
      // a double click goes back to the column's own width
      fireEvent.doubleClick(edge);
      expect(colWidth("名称")).toBe("150px");
    });
  });
});
