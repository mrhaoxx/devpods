import { useSearchParams } from "react-router-dom";
import { DevPodTable } from "./DevPodTable";
import { DevPodDrawer } from "./DevPodDrawer";

export function MyDevPodsPage() {
  const [params, setParams] = useSearchParams();
  const open = params.get("new") === "1";
  return (
    <>
      <DevPodTable scope="mine" />
      {open && (
        <DevPodDrawer
          mode="create"
          open
          onClose={() =>
            setParams(
              (cur) => {
                const next = new URLSearchParams(cur);
                next.delete("new");
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
