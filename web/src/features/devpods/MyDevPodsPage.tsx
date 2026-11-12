import { OtherClusters } from "../clusters/OtherClusters";
import { useSearchParams } from "react-router-dom";
import { DevPodTable } from "./DevPodTable";
import { DevPodDrawer } from "./DevPodDrawer";

export function MyDevPodsPage() {
  const [params, setParams] = useSearchParams();
  const open = params.get("new") === "1";
  return (
    <>
      <DevPodTable scope="mine" />
      <OtherClusters />
      {open && (
        <DevPodDrawer
          mode="create"
          initialImage={params.get("image") ?? undefined}
          open
          onClose={() =>
            setParams(
              (cur) => {
                const next = new URLSearchParams(cur);
                next.delete("new");
                next.delete("image");
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
