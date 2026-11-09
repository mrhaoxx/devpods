import { useQuery, useQueryClient, useMutation } from "@tanstack/react-query";
import { auditPresence, listAllDevPods, me, patchDevPod } from "../api";
import { DevPodTable } from "../components/DevPodTable";
import { BackLink, Shell } from "../ui";

export default function AdminDevPods() {
  const qc = useQueryClient();
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const q = useQuery({ queryKey: ["admin-devpods"], queryFn: listAllDevPods, refetchInterval: 8000 });
  const presenceQ = useQuery({
    queryKey: ["presence-all"],
    queryFn: () => auditPresence(true),
    enabled: !!meQ.data?.features.audit,
    refetchInterval: 10_000,
  });
  const toggle = useMutation({
    mutationFn: ({ name, running }: { name: string; running: boolean }) => patchDevPod(name, running),
    onSettled: () => qc.invalidateQueries({ queryKey: ["admin-devpods"] }),
  });

  return (
    <Shell wide>
      <BackLink />
      <header className="mb-4 mt-3">
        <h1 className="mono text-xl font-semibold tracking-tight">All DevPods</h1>
      </header>
      {meQ.data && (
        <DevPodTable
          items={q.data?.devpods ?? []}
          pods={q.data?.pods ?? {}}
          me={meQ.data}
          presence={presenceQ.data?.items}
          showOwner
          onToggle={(name, running) => toggle.mutate({ name, running })}
          toggling={toggle.isPending}
        />
      )}
    </Shell>
  );
}
