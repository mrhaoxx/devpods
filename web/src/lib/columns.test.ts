import { describe, expect, it } from "vitest";
import { ALL_COLUMNS, ColumnKey, visibleColumns } from "./columns";

const all = [...ALL_COLUMNS];

describe("visibleColumns", () => {
  it("shows everything on a wide screen", () => {
    expect(visibleColumns(1200, all)).toEqual(all);
  });
  it("hides by priority: image, rdma, net, up", () => {
    const at = (w: number) => visibleColumns(w, all);
    expect(all.filter((c) => !at(1000).includes(c))).toEqual(["image"]);
    expect(all.filter((c) => !at(900).includes(c))).toEqual(["rdma", "image"]);
    expect(all.filter((c) => !at(800).includes(c))).toEqual(["net", "rdma", "image"]);
    expect(all.filter((c) => !at(700).includes(c))).toEqual(["net", "rdma", "up", "image"]);
  });
  it("never shows columns that are not available", () => {
    const avail: ColumnKey[] = ["name", "phase", "cpu", "up"];
    expect(visibleColumns(1600, avail)).toEqual(avail);
    expect(visibleColumns(600, avail)).toEqual(["name", "phase", "cpu"]);
  });
});
