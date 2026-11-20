// The DevPod CLI over SSH: how to reach it through the gateway, each line
// copyable — logged in the way a DevPod is (the port, the route's suffix).
import { sshOf, type Me } from "../../data/api";
import { CopyBlock, CopyField } from "../../design/Copy";
import { Panel } from "../../design/Panel";

export function Cli({ me }: { me: Me }) {
  if (!me.cli) return null;
  const ssh = sshOf(me);
  const host = ssh?.host || "<gateway>";
  const port = ssh?.port ?? 22;
  const login = `${me.user}+${me.cli}${ssh?.loginSuffix ? `+${ssh.loginSuffix}` : ""}`;
  const base = `ssh ${port !== 22 ? `-p ${port} ` : ""}${login}@${host}`;
  const line = (cmd: string, what: string) => (
    <div className="space-y-0.5">
      <div className="text-[11px] text-muted">{what}</div>
      <CopyField value={cmd} />
    </div>
  );
  return (
    <Panel title="命令行（SSH）">
      <div className="space-y-3 p-3 text-xs">
        <p className="text-muted">
          不开网页也能在终端里管理 DevPod：列出、查看、创建、启动和休眠、删除、看事件、改协作者等。用上面登记的 SSH 公钥登录，不需要访问令牌
          {me.features.clusters && "；命令加 --cluster 指定集群"}。
        </p>
        {line(base, "交互式：进入 devpod> 提示符，逐条输入命令")}
        {line(`${base} list`, "执行一条命令：列出你的 DevPod")}
        {line(`${base} help`, "全部命令和用法（help <命令> 看某一条）")}
        <div className="space-y-1">
          <div className="text-[11px] text-muted">写进 ~/.ssh/config 后可简写为 ssh devpod list：</div>
          <CopyBlock value={`Host devpod\n    HostName ${host}\n    Port ${port}\n    User ${login}`} />
        </div>
      </div>
    </Panel>
  );
}
