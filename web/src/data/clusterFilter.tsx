// Which clusters a page shows: all of them, or one — the address says
// (?cluster=), so a link keeps it; the browser remembers the last choice
// and brings it back to an address that does not say.
import { useCallback, useEffect } from "react";
import { useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { listClusters } from "../features/clusters/api";

/** useClusterList: the clusters of this console (none for a console of its own one). */
export const useClusterList = () => useQuery({ queryKey: ["clusters"], queryFn: listClusters, refetchInterval: 15_000, staleTime: 10_000 });

const KEY = "devpod.clusterFilter";
const recall = () => {
  try {
    return window.localStorage.getItem(KEY) ?? "";
  } catch {
    return "";
  }
};
const keep = (c: string) => {
  try {
    if (c) window.localStorage.setItem(KEY, c);
    else window.localStorage.removeItem(KEY);
  } catch {
    /* not kept: no matter */
  }
};

export function useClusterFilter() {
  const [params, setParams] = useSearchParams();
  const clusters = useClusterList().data?.items ?? [];
  const known = (c: string) => clusters.some((x) => x.name === c);
  const asked = params.get("cluster") ?? "";
  // a cluster that is gone is no filter
  const filter = asked && (clusters.length === 0 || known(asked)) ? asked : "";

  useEffect(() => {
    if (clusters.length === 0) return;
    const remembered = recall();
    if (remembered && !known(remembered)) keep("");
    if (params.has("cluster") || !remembered || !known(remembered)) return;
    setParams(
      (cur) => {
        const next = new URLSearchParams(cur);
        next.set("cluster", remembered);
        return next;
      },
      { replace: true },
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [params, clusters.map((c) => c.name).join(",")]);

  const setFilter = useCallback(
    (c: string) => {
      keep(c);
      setParams((cur) => {
        const next = new URLSearchParams(cur);
        if (c) next.set("cluster", c);
        else next.delete("cluster");
        return next;
      });
    },
    [setParams],
  );
  const matches = useCallback((item: { cluster?: string }) => !filter || item.cluster === filter, [filter]);
  return { filter, setFilter, several: clusters.length >= 2, clusters, matches };
}
