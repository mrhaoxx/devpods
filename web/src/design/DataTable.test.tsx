import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { useState } from "react";
import { DataTable, phoneHidden, rowWindow } from "./DataTable";

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

  it("the bulk bar floats at the bottom: nothing above the table moves; Esc clears", () => {
    const { container } = render(<Harness />);
    const before = container.firstElementChild!.firstElementChild; // what the table sits under
    fireEvent.click(screen.getByLabelText("选择 a"));
    const bar = screen.getByRole("toolbar", { name: "批量操作" });
    expect(bar.closest(".fixed")).toBeTruthy(); // out of the flow, at the window's bottom
    expect(container.firstElementChild!.firstElementChild).toBe(before);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("toolbar", { name: "批量操作" })).toBeNull();
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

  it("on a phone, by default, only the most wanted columns; elsewhere every one", () => {
    const metas = [1, 2, 3, 5].map((p) => ({ id: `c${p}`, priority: p }));
    expect(phoneHidden(400, metas)).toEqual(["c3", "c5"]);
    expect(phoneHidden(1200, metas)).toEqual([]);
    expect(phoneHidden(0, metas)).toEqual([]); // width not known yet
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

    it("dragging a column's edge sets its width — that column's alone — and it is remembered", () => {
      table();
      const edge = screen.getByRole("separator", { name: "调整 名称 列宽" });
      // by pointer events: a finger drags it as a mouse does
      const touch = { pointerId: 7, pointerType: "touch" };
      fireEvent.pointerDown(edge, { clientX: 100, ...touch });
      fireEvent.pointerMove(document, { clientX: 140, ...touch });
      expect(document.body.classList.contains("col-resizing")).toBe(true);
      fireEvent.pointerUp(document, { clientX: 140, ...touch });
      expect(document.body.classList.contains("col-resizing")).toBe(false);
      expect(colWidth("名称")).toBe("190px");
      expect(colWidth("CPU")).toBe("90px");
      expect(JSON.parse(localStorage.getItem("devpod.columns.t") ?? "{}").name).toBe(190);
      // a double click goes back to the column's own width
      fireEvent.doubleClick(edge);
      expect(colWidth("名称")).toBe("150px");
    });

    it("the room the columns leave goes to a filler, never into the columns", () => {
      const { container } = table();
      const filler = container.querySelector("thead th[data-filler]") as HTMLElement;
      expect(filler).toBeTruthy();
      expect(filler.style.width).toBe("");
      expect(screen.getByRole("table").style.width).toBe("100%");
      expect(container.querySelectorAll("tbody tr:first-child td[data-filler]")).toHaveLength(1);
    });

    it("an edge moves by the keyboard too", () => {
      table();
      const edge = screen.getByRole("separator", { name: "调整 CPU 列宽" });
      fireEvent.keyDown(edge, { key: "ArrowRight" });
      expect(colWidth("CPU")).toBe("98px");
      fireEvent.keyDown(edge, { key: "ArrowLeft", shiftKey: true });
      expect(colWidth("CPU")).toBe("66px");
      expect(JSON.parse(localStorage.getItem("devpod.columns.t") ?? "{}").cpu).toBe(66);
    });
  });

  describe("right-click menu", () => {
    function Menued({ onAct }: { onAct: (what: string) => void }) {
      return (
        <DataTable
          data={rows}
          columns={columns}
          getRowId={(r) => r.name}
          sorting={[]}
          onSortingChange={() => {}}
          selectable
          rowMenu={(picked, bulk) => [{ label: `${bulk ? "批量" : "单个"} ${picked.map((r) => r.name).join(",")}`, onSelect: () => onAct(picked.map((r) => r.name).join(",")) }]}
        />
      );
    }
    const rowOf = (name: string) => screen.getByLabelText(`选择 ${name}`).closest("tr")!;

    it("on a row: that row's own actions, and the selection is left as it was", () => {
      const acted: string[] = [];
      render(<Menued onAct={(w) => acted.push(w)} />);
      fireEvent.click(screen.getByLabelText("选择 a"));
      fireEvent.click(screen.getByLabelText("选择 c"));
      fireEvent.contextMenu(rowOf("b"));
      const item = within(screen.getByRole("menu")).getByRole("menuitem", { name: "单个 b" });
      fireEvent.click(item);
      expect(acted).toEqual(["b"]);
      expect(screen.getByText("已选 2")).toBeTruthy();
    });

    it("on a row of the selection: the selection's actions", () => {
      render(<Menued onAct={() => {}} />);
      fireEvent.click(screen.getByLabelText("选择 a"));
      fireEvent.click(screen.getByLabelText("选择 c"));
      fireEvent.contextMenu(rowOf("c"));
      const menu = within(screen.getByRole("menu"));
      expect(menu.getByText("已选 2")).toBeTruthy();
      expect(menu.getByRole("menuitem", { name: "批量 a,c" })).toBeTruthy();
    });

    it("with Shift: the browser's own menu", () => {
      render(<Menued onAct={() => {}} />);
      fireEvent.contextMenu(rowOf("a"), { shiftKey: true });
      expect(screen.queryByRole("menu")).toBeNull();
    });
  });

  describe("choosing columns", () => {
    afterEach(() => localStorage.clear());
    type R = { name: string; cpu: number; node: string };
    const data: R[] = [{ name: "a", cpu: 1, node: "g1" }];
    const cols: ColumnDef<R, unknown>[] = [
      { id: "name", header: "名称", accessorKey: "name", meta: { pin: "left", width: 150 } },
      { id: "cpu", header: "CPU", accessorKey: "cpu", meta: { width: 90 } },
      { id: "node", header: "节点", accessorKey: "node", meta: { width: 90 } },
      { id: "actions", header: "", cell: () => "⋯", meta: { pin: "right", width: 36 } },
    ];
    const table = () => render(<DataTable data={data} columns={cols} getRowId={(r) => r.name} sorting={[]} onSortingChange={() => {}} storageKey="t" selectable />);
    const openMenu = () => fireEvent.keyDown(screen.getByRole("button", { name: "选择列" }), { key: "Enter" });

    it("the 列 menu hides a column and brings it back; the choice is remembered", () => {
      const first = table();
      expect(screen.queryByText(/显示全部列/)).toBeNull();
      expect(screen.getByRole("columnheader", { name: /节点/ })).toBeTruthy();
      openMenu();
      const menu = within(screen.getByRole("menu"));
      expect(menu.queryByRole("menuitemcheckbox", { name: "名称" })).toBeNull(); // pinned: always shown
      fireEvent.click(menu.getByRole("menuitemcheckbox", { name: "节点" }));
      expect(screen.queryByRole("columnheader", { name: /节点/ })).toBeNull();
      expect(JSON.parse(localStorage.getItem("devpod.columns-hidden.t") ?? "[]")).toEqual(["node"]);
      first.unmount();
      table();
      expect(screen.queryByRole("columnheader", { name: /节点/ })).toBeNull();
      openMenu();
      fireEvent.click(within(screen.getByRole("menu")).getByRole("menuitem", { name: "恢复默认" }));
      expect(screen.getByRole("columnheader", { name: /节点/ })).toBeTruthy();
      expect(localStorage.getItem("devpod.columns-hidden.t")).toBeNull();
    });

    it("pinned columns stay put when the table scrolls sideways", () => {
      table();
      const name = screen.getByRole("columnheader", { name: /名称/ }) as HTMLElement;
      expect(name.style.position).toBe("sticky");
      expect(name.style.left).toBe("28px"); // after the checkbox
      const last = screen.getAllByRole("columnheader").at(-1) as HTMLElement;
      expect(last.style.position).toBe("sticky");
      expect(last.style.right).toBe("0px");
    });
  });
});

describe("picking rows by clicking", () => {
  it("a click on a row picks it; a click on the empty part of the list lets go", () => {
    const { container } = render(<DataTable fill selectable data={rows} columns={columns} getRowId={(r) => r.name} sorting={[]} onSortingChange={() => {}} />);
    fireEvent.click(screen.getByText("b"));
    expect(screen.getByText("已选 1")).toBeTruthy();
    fireEvent.click(screen.getByRole("columnheader", { name: /名称/ })); // the header is not the empty part
    expect(screen.getByText("已选 1")).toBeTruthy();
    fireEvent.click(container.querySelector("[data-table-scroller]")!);
    expect(screen.queryByText("已选 1")).toBeNull();
  });
});

describe("a list page's table (fill)", () => {
  it("works out the rows in sight, and all of them while its height is unknown", () => {
    expect(rowWindow(1000, { top: 0, height: 0 }, 29, 29)).toEqual({ first: 0, last: 1000 });
    expect(rowWindow(1000, { top: 0, height: -1 }, 29, 29)).toEqual({ first: 0, last: 60 }); // not read yet: a screenful
    // scrolled 100 rows down, 20 rows tall under a one-row header: rows 100–118 show, 12 more either side
    expect(rowWindow(1000, { top: 2900, height: 580 }, 29, 29)).toEqual({ first: 88, last: 131 });
    expect(rowWindow(1000, { top: 29_000, height: 580 }, 29, 29)).toEqual({ first: 988, last: 1000 });
  });

  it("draws only those rows, with room for the rest, and follows the scroll", async () => {
    const many = Array.from({ length: 1000 }, (_, i) => ({ name: `dev${i}`, cpu: i }));
    const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight"); // jsdom: none (Element's)
    Object.defineProperty(HTMLElement.prototype, "clientHeight", {
      configurable: true,
      get() {
        return (this as HTMLElement).hasAttribute("data-table-scroller") ? 290 : 0;
      },
    });
    try {
      const { container } = render(<DataTable fill data={many} columns={columns} getRowId={(r) => r.name} sorting={[]} onSortingChange={() => {}} />);
      const drawn = () => [...container.querySelectorAll("tbody tr[data-row]")].map((r) => r.getAttribute("data-row"));
      expect(drawn().length).toBeLessThan(40);
      expect(drawn()[0]).toBe("dev0");
      const scroller = container.querySelector<HTMLElement>("[data-table-scroller]")!;
      scroller.scrollTop = 29 * 500;
      fireEvent.scroll(scroller);
      await waitFor(() => expect(drawn()).toContain("dev500"));
      expect(drawn()).not.toContain("dev0");
      expect(drawn().length).toBeLessThan(40);
    } finally {
      if (own) Object.defineProperty(HTMLElement.prototype, "clientHeight", own);
      else delete (HTMLElement.prototype as { clientHeight?: number }).clientHeight;
    }
  });
});
