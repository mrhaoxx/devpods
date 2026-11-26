// The record of what was done (操作记录): how its actions, kinds and ways
// in read in the UI.

export const ACTIONS: Record<string, string> = {
  create: "创建",
  delete: "删除",
  start: "启动",
  stop: "停止",
  queue: "排队",
  dequeue: "取消排队",
  edit: "修改",
  pin: "绑定节点",
  unpin: "解绑节点",
  redeploy: "重新部署",
  yaml: "编辑 YAML",
  "http-ports": "端口",
  snapshot: "快照",
  "snapshot-delete": "删除快照",
  "snapshot-retry": "重试快照",
  "rebuild-from-config": "按配置重建",
  hibernate: "休眠",
  "queue-start": "队列启动",
  "requeue-unschedulable": "调度不上，重新排队",
  "auto-unpin": "去掉自动绑定",
  submit: "提交",
  resubmit: "重新提交",
  cancel: "取消",
  preempted: "被抢占",
  requeue: "重新排队",
  end: "结束",
  "reservable-on": "开放预约",
  "reservable-off": "关闭预约",
  closed: "机器关闭",
};

export const KINDS: Record<string, string> = { devpod: "DevPod", job: "任务", machine: "机器" };

export const actionLabel = (a: string) => ACTIONS[a] ?? a;
export const kindLabel = (k: string) => KINDS[k] ?? k;

/** viaLabel: how an action came — 网页, 命令行, dp, 令牌 <id>, 系统. */
export function viaLabel(via: string): string {
  if (via.startsWith("token:")) return `令牌 ${via.slice("token:".length)}`;
  return { web: "网页", cli: "命令行", dp: "dp", system: "系统" }[via] ?? via;
}
