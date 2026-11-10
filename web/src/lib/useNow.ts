import { useEffect, useState } from "react";

/** useNow re-renders every `everyMs` with the current time. */
export function useNow(everyMs = 1000): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), everyMs);
    return () => clearInterval(t);
  }, [everyMs]);
  return now;
}
