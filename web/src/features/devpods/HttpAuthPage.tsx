// /http-auth?host=&rd= — where the gateway sends a browser that opened a
// private DevPod port without its cookie. Being here means the console
// login is done, so ask for a ticket and go back to the port.
import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ApiFailure, httpTicket } from "../../data/api";
import { EmptyState } from "../../design/EmptyState";

export function HttpAuthPage() {
  const [q] = useSearchParams();
  const host = q.get("host") ?? "";
  const rd = q.get("rd") ?? "/";
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    httpTicket(host, rd).then(
      (r) => live && window.location.replace(r.url),
      (e) => live && setError(e instanceof ApiFailure && e.status === 403 ? "这个地址不存在，或者你没有访问权限。只有 DevPod 的所有者、协作者和管理员可以打开未公开的端口。" : e instanceof ApiFailure ? e.body.message : String(e)),
    );
    return () => {
      live = false;
    };
  }, [host, rd]);
  return error ? <EmptyState tone="error" title="无法打开这个端口" hint={error} /> : <EmptyState title="正在打开…" hint={host} />;
}
