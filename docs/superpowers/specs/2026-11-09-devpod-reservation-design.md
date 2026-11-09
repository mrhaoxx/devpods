# DevPod 节点预约

日期：2026-11-09　状态：设计已逐节确认，待审阅

相关：
- `2026-11-09-devpod-console-ui-design.md` §5.7 的预约界面，以及 §6.2 的接口。
- Mockup：`2026-11-09-console-ui-mockups/reservation.html`。其中的"本周剩余额度"已取消（本 spec §3.4）。

---

## 1. 目标与非目标

### 1.1 目标

管理员可以把节点设为"可预约"。普通用户预约一段时间后，这段时间里这台节点只给他的 DevPod 用；预约结束后，他在这台节点上的 DevPod 自动暂停（休眠）。

已经确认的产品规则：

| 规则 | 决定 |
|---|---|
| 预约单位 | 整台节点 |
| 独占范围 | 只在 DevPod 之间独占：预约期间别人的 DevPod 不能调度上去，已有的会被休眠；非 DevPod 的工作负载不管（不动节点 taint） |
| 生效方式 | 先到先得，时间段不冲突就立即成功，不需要审批 |
| 配额 | 不做 |
| 空闲时段 | 没有预约时任何人的 DevPod 都可以用这台节点；预约开始前 30 分钟通知节点上的其他人，开始时把他们的 DevPod 休眠 |
| 使用方式 | 手动启动：预约只给资格，用户在预约时段内新建或启动 DevPod 时，自己选这台节点 |
| 谁能用 | 只有预约人自己名下的 DevPod；协作者通过 SSH 进入预约人的 DevPod 使用 |
| 结束 | 到期、提前释放、被管理员取消，三种情况都会休眠预约人在这台节点上的 DevPod |
| 取消 | 用户可以取消自己的预约（已经开始的就是提前释放）；管理员可以取消任何人的预约 |
| 界面 | 时间轴，可以拖动选择时间段，也可以取消（见 UI spec §5.7） |

### 1.2 非目标

- 不做按 GPU 卡或按资源量的预约。
- 不做审批流程，不做配额，不做排队或候补。
- 预约开始时不会自动启动 DevPod。
- 不管节点上非 DevPod 的工作负载。
- 不做跨节点的组合预约。

---

## 2. 数据模型

### 2.1 节点"可预约"标记

Node 标签 `devpod.io/reservable=true`。管理员在节点页切换开关，webui 去 patch 这个标签。

取消"可预约"时，这台节点上还没结束的预约不受影响，照常执行到结束；webui 会在开关旁提示"还有 N 个未结束的预约"。

### 2.2 `Reservation` CRD（集群级，`devpod.io/v1alpha1`）

```go
type ReservationSpec struct {
    Node  string      `json:"node"`  // 节点名
    User  string      `json:"user"`  // 预约人（User 名）
    Start metav1.Time `json:"start"`
    End   metav1.Time `json:"end"`
    // Cancelled 在取消或提前释放时由 webui 写入；controller 看到后按"结束"处理。
    Cancelled *ReservationCancel `json:"cancelled,omitempty"`
    // Extends 是被延长的那一段的名字。延长不修改原预约，而是新建一段，
    // 它的 Start 等于原预约的 End（§3.2）。
    Extends string `json:"extends,omitempty"`
}

type ReservationCancel struct {
    By string      `json:"by"` // 操作人
    At metav1.Time `json:"at"`
}

type ReservationStatus struct {
    // Phase: Upcoming | Active | Ended | Cancelled
    Phase            ReservationPhase `json:"phase,omitempty"`
    NotifiedClearAt  *metav1.Time     `json:"notifiedClearAt,omitempty"`  // 已发出清场预告
    ClearedDevPods   []string         `json:"clearedDevPods,omitempty"`   // 开始时被休眠的他人 DevPod
    HibernatedAtEnd  []string         `json:"hibernatedAtEnd,omitempty"`  // 结束时被休眠的预约人 DevPod
}
```

- 名字由 webui 生成：`<node>-<user>-<随机后缀>`。
- printer columns：Node、User、Start、End、Phase。
- 校验规则：`End > Start`；`Start`、`End` 必须对齐到粒度（§3.1）。

### 2.3 DevPod 的新增字段

- `spec.nodeName`（可选）：用户指定的节点。渲染成 Pod 的 `nodeSelector: {kubernetes.io/hostname: <nodeName>}`。它属于用户意图，计入 Pod 指纹；修改它会重建 Pod，和现在编辑 DevPod 的行为一致。
- `status.conditions` 新增三种类型：

| type | 什么时候为 True | message 示例 |
|---|---|---|
| `NodeReserved` | `spec.nodeName` 指向的节点正被别人预约，Pod 没有创建 | `g206 被 alice 预约至 17:00` |
| `ReservationClearing` | DevPod 在一台即将开始他人预约的节点上运行（开始前 30 分钟到开始） | `g206 将于 14:30 被 alice 预约，届时自动休眠`；没有持久化存储时加上"休眠后容器内数据丢失" |
| `ReservationEnding` | 预约人的 DevPod 所在的预约，离结束不到 30 分钟 | `预约将于 17:00 结束，届时自动休眠` |

---

## 3. 规则与并发

### 3.1 规则参数

集群级配置，webui 和 controller 用同一组启动参数，chart 中写在 values 里：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--reservation-granularity` | 30m | 开始和结束都要对齐到这个粒度 |
| `--reservation-max-duration` | 24h | 单次预约的最长时长；0 = 不限 |
| `--reservation-max-advance` | 336h（14 天） | 最多提前多久预约；0 = 不限 |
| `--reservation-clear-notice` | 30m | 提前多久发清场预告 |
| `--reservation-ending-notice` | 30m,5m | 结束前提醒的时间点 |
| `--reservation-retention` | 720h（30 天） | 已结束或已取消的预约保留多久 |

时区固定为 Asia/Shanghai，只用于界面显示和对齐；存储一律用 UTC。

### 3.2 写入路径（webui）

- **新建**：
  - 校验：节点存在且可预约；时间对齐；时长和提前量没超出规则；开始时间不早于现在往前一个粒度，也就是允许"从当前这一格开始"。
  - 校验通过后创建 Reservation，然后做冲突裁决（§3.3）。
- **修改**（只能改还没开始的预约）：先取消原预约，再按新时段新建一个。排队顺序按新建的时间算，相当于重新预约。
- **延长**：新建一段 `[原 End, 新 End)` 的 Reservation，`spec.extends` 指向原预约，再做冲突裁决。冲突时只是延长失败，原预约不受影响。延长后的这一段也可以继续延长，形成一条链；界面把同一个人首尾相接的几段合并成一块显示。
- **取消**：写入 `spec.cancelled`。还没开始的预约，相当于作废；已经开始的就是提前释放。用户只能取消自己的预约，管理员可以取消任何人的。

### 3.3 冲突裁决（不需要 webhook）

1. webui 创建或更新 Reservation。
2. **直接向 API server 读**（不走缓存）这台节点上所有未取消、并且和本次时间段重叠的 Reservation。
3. 按 `(creationTimestamp, name)` 排序，排第一的保留。如果本次新建的不是第一个，就把自己删掉，然后返回 409，说明和谁冲突。

两个人同时抢同一段时，两边都会读到对方，只有排序靠前的那个会留下。修改和延长都是新建对象，所以所有冲突都用这一条规则裁决，没有例外。

---

## 4. 调度约束

### 4.1 没指定节点的 DevPod

controller 创建 Pod 时，加一条必须满足的节点亲和性：`kubernetes.io/hostname NotIn [当前正被别人预约的节点]`。只看当前时刻，不往后看。

这一条**在计算 Pod 指纹之后才附加，不计入指纹**（和 supervisor 初始化容器不计入指纹的做法一致），所以预约变化永远不会触发正在运行的 Pod 重建。

### 4.2 指定了节点的 DevPod

- **webui**：新建、启动或编辑时，如果 `spec.nodeName` 正被别人预约，就返回 409 并说明原因。
- **controller 兜底**（应对竞争）：
  - `spec.running=true`，但节点正被别人预约：不创建 Pod，把 `NodeReserved` 设为 True，phase 显示为 Pending，message 写原因。
  - 节点空出来后，自动按正常流程创建 Pod。

### 4.3 预约人自己

预约人名下、`spec.nodeName` 等于预约节点的 DevPod，在预约时段内照常调度。协作者不单独享有资格。

---

## 5. Controller 时间线

新增 `ReservationReconciler`，和 DevPodReconciler 在同一个 controller-manager 进程里。每次协调完，用 RequeueAfter 排到下一个时间点。

| 时间点 | 动作 |
|---|---|
| 开始前 `clear-notice` | 找出节点上非预约人的运行中 DevPod（按 Pod 的 `spec.nodeName`），把它们的 `ReservationClearing` 设为 True，发 k8s Event，写入 `status.notifiedClearAt` |
| 开始 | phase 变为 Active；把这些 DevPod 休眠：`spec.running=false`，加注解 `devpod.io/hibernated-by: reservation/<name>`；写入 `status.clearedDevPods`；清除它们的 `ReservationClearing` |
| 结束前的每个 `ending-notice` 时间点 | 把预约人在这台节点上运行中的 DevPod 的 `ReservationEnding` 设为 True |
| 结束、`spec.cancelled` 或被删除 | 如果存在同一个人、未取消、`Start` 等于本段 `End` 的后续段（延长），就不休眠，直接把 phase 变为 Ended；否则休眠预约人在这台节点上的 DevPod（注解同上），写入 `status.hibernatedAtEnd`，phase 变为 Ended 或 Cancelled。两种情况都要清除相关条件。取消一个还有后续段的预约时，同时取消它的全部后续段 |
| 结束后过了 `retention` | 删除这个 Reservation |

- **幂等**：每一步只处理状态还没变过的对象，也就是幂等的。controller 重启后，按当前时间补做错过的步骤：比如重启时已经过了开始时间，会直接清场，然后补发 Event。
- **休眠**：沿用现有语义。Pod 被删除，PVC 保留；没有持久化存储的 DevPod，容器内的数据会丢失，所以清场预告和界面都要明确提示这一点。
- **DevPodReconciler 侧**：渲染时附加 §4.1 的排除规则，执行 §4.2 的兜底；watch Reservation，预约的开始和结束会触发相关 DevPod 重新协调。

---

## 6. 通知与展示

- **通知不单独存储**。`GET /api/me/notices` 在请求时现场算出：
  - 我名下 DevPod 上的 `ReservationClearing`、`ReservationEnding`、`NodeReserved` 条件；
  - 我的预约被管理员取消（`spec.cancelled.by` 不是我本人，且发生在最近 7 天内）；
  - 我名下被预约清场休眠的 DevPod（看注解）。
- 这些状态变化时，webui 根据 watch 到的变化，向这个用户的 SSE 连接推送 `notice` 事件。
- 已读状态保存在浏览器本地，按用户名分开存。UI spec 中的 `POST /api/me/notices/{id}/read` 删除。
- **gateway 登录 banner**：模板新增字段 `.DevPodNotice`，取 DevPod 上为 True 的 `ReservationClearing` 或 `ReservationEnding` 的 message。默认 banner 在这个字段不为空时附加一行。

---

## 7. 接口（webui）

UI spec §6.2 中与预约相关的部分以这里为准：

| 接口 | 说明 |
|---|---|
| `GET /api/reservations?from=&to=[&node=]` | 时间轴，所有人可见，包括预约人的用户名 |
| `POST /api/reservations` | `{node, start, end}`。违反规则返回 400；冲突返回 409，并带上冲突对象 |
| `PATCH /api/reservations/{id}` | `{start?, end?}`。对还没开始的预约，等于取消后重新预约；对已经开始的预约，只能增大 `end`，这时新建一段延长段（§3.2）。返回的是合并后的视图 |
| `DELETE /api/reservations/{id}` | 写入 `spec.cancelled`：取消或提前释放 |
| `GET /api/reservations/rules` | 规则参数（§3.1），替代 UI spec 中的 `/api/me/reservation-quota` |
| `PUT /api/admin/nodes/{n}/reservable` | `{reservable: bool}`，patch 节点标签 |
| `GET /api/me/notices` | §6 |

新建、启动、编辑 DevPod 的接口加上 §4.2 的检查。`spec.nodeName` 可以通过新建和编辑接口设置：值必须是一个存在的节点；如果节点可预约，还要满足 §4.2。

---

## 8. 部署

- **CRD**：新增 `Reservation`；`DevPod.spec.nodeName`；新增的条件类型不需要改 schema。
- **controller**：
  - RBAC：reservations 的 get/list/watch/update/delete，reservations/status 的 update/patch，nodes 的 get/list/watch；
  - 新启动参数见 §3.1。
- **webui**：
  - RBAC：reservations 的 create/get/list/watch/update/delete，nodes 的 patch（只改标签）；
  - 参数同上。
- **gateway**：只改 banner 模板，不需要新增 RBAC（它已经能读 DevPod）。
- **chart**：values 新增 `reservation.*`，存放规则参数。

---

## 9. 测试

- **单元测试**：
  - 冲突裁决的排序；
  - 规则校验：对齐、时长、提前量、从当前这一格开始；
  - Pod 指纹不受 §4.1 的排除规则影响；
  - 通知的计算；
  - 时间线下一个时间点的计算。
- **envtest**：
  - 两个人并发抢同一时段，只成功一个；延长段和别人的新预约并发时只成功一个，原预约不受影响；
  - 首尾相接的延长段在交界处不休眠；取消原预约时，后续段一并取消；
  - controller 时间线：清场预告条件 → 开始时清场（DevPod 的 `running=false` 并带注解）→ 结束前提醒 → 到期休眠预约人的 DevPod；
  - 管理员取消，等同于提前释放；
  - controller 重启后补做错过的步骤；
  - 指定了别人预约中节点的 DevPod：不创建 Pod，带上 `NodeReserved`，节点空出来后自动创建；
  - 没指定节点的 DevPod 的 Pod 带上排除规则，并且预约变化不会让它被重建。
- **e2e**：沿用隔离方案，在 zjusct 的 `devpod-audit-it` 上，把粒度设为 1 分钟，走一遍"他人 DevPod 被清场 → 预约人启动 → 到期休眠"。

---

## 10. 对其他文档的影响

- **UI spec**：去掉预约配额的相关内容；`/api/me/reservation-quota` 改成 `/api/reservations/rules`；删除 `POST /api/me/notices/{id}/read`；§5.7 的规则与本 spec §1.1 同步。
- **QUICKSTART**：新增"节点预约"一节，面向管理员（如何设置可预约节点）和用户（如何预约、到期会发生什么）。
