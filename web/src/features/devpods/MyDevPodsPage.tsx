import { OtherClusters } from "../clusters/OtherClusters";
import { useSearchParams } from "react-router-dom";
import { DevPodTable } from "./DevPodTable";
import { NewDevPodDrawer } from "./NewDevPodDrawer";

export function MyDevPodsPage() {
  const [params, setParams] = useSearchParams();
  const open = params.get("new") === "1";
  return (
    <>
      <DevPodTable scope="mine" />
      <OtherClusters />
      {open && (
        <NewDevPodDrawer
          config={params.get("config") ?? undefined}
          image={params.get("image") ?? undefined}
          onClose={() =>
            setParams(
              (cur) => {
                const next = new URLSearchParams(cur);
                for (const k of ["new", "image", "config"]) next.delete(k);
                return next;
              },
              { replace: true },
            )
          }
        />
      )}
    </>
  );
}
