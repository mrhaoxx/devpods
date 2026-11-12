import { useSearchParams } from "react-router-dom";
import { DevPodTable } from "./DevPodTable";
import { NewDevPodDrawer } from "./NewDevPodDrawer";

export function MyDevPodsPage() {
  const [params, setParams] = useSearchParams();
  const open = params.get("new") === "1";
  return (
    <>
      <DevPodTable scope="mine" />
      {open && (
        <NewDevPodDrawer
          config={params.get("config") ?? undefined}
          image={params.get("image") ?? undefined}
          in={params.get("in") ?? undefined}
          onClose={() =>
            setParams(
              (cur) => {
                const next = new URLSearchParams(cur);
                for (const k of ["new", "image", "config", "in"]) next.delete(k);
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
