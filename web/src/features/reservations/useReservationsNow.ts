// The reservations around now, for the notices on DevPods (reservationNotes):
// loaded only where reservations are on, again every minute.
import { useQuery } from "@tanstack/react-query";
import { getReservationRules, listReservations } from "../../data/api";
import { useMe } from "../../data/queries";
import type { ReservationsNow } from "../devpods/status";

export function useReservationsNow(): ReservationsNow | undefined {
  const on = !!useMe().data?.features.reservations;
  const rules = useQuery({ queryKey: ["reservation-rules"], queryFn: getReservationRules, staleTime: 300_000, retry: false, enabled: on });
  const noticeMs = (rules.data?.clearNoticeSeconds ?? 0) * 1000;
  const items = useQuery({
    queryKey: ["reservations", "now", noticeMs],
    queryFn: async () => {
      const now = Date.now();
      // a held node's hold, and what starts or ends within the notice
      const res = await listReservations(new Date(now - 60_000).toISOString(), new Date(now + noticeMs + 60_000).toISOString());
      return { items: res.items, now };
    },
    enabled: on && !!rules.data,
    refetchInterval: 60_000,
    staleTime: 30_000,
  });
  if (!items.data) return undefined;
  return { items: items.data.items, noticeMs, now: Date.now() };
}
