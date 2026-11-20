# KubeCause — Kubernetes 故障诊断 Agent（Slice 9）

KubeCause 是一个 Kubernetes 故障诊断 Agent。用户选定 namespace 和 Pod 或 Deployment，描述故障现象，Agent 自主选择只读工具收集证据，检索排障手册，输出诊断结论和建议。演示环境支持人工审批后的变更：用一组通用 Patch 改指定 Deployment 的白名单字段（镜像、内存 limit/request、CPU limit/request、副本数），并验证变更后的运行状态。

Slice 9 修的是用户在 2026-11-20 反馈的三个问题：**整页滚动**（输入框还是被顶走，时间线看不到）、**只有 OOM 能出修复建议**（ImagePullBackOff 没有卡）、**每个故障都要单独写代码吗**（执行器只认内存）。核心是两件事：**布局锁死一屏**（`.app` 从 `min-height:100vh` 改成 `height:100vh; overflow:hidden`，三栏各自内部滚动）和**执行器改成通用 Patch**（建议卡、校验、执行、提案、审批卡全部按 op/path/value 走，不再按故障类型分分支，OOM、ImagePullBackOff、主动换镜像版本都走同一套流程）。

Slice 1/2/3/4/5/6/7/8 已有的东西没有重写：SQLite 表（只给 change_requests 加了一列 patches_json，老库启动时自动补列）、HTTP 接口、Worker 状态机、Agent 循环的校验和恢复、fake 客户端、桩模型、真模型适配器、FTS5 检索、条件 Patch 与 rollout 验证、对话会话、发现工具、React 前端，全部保留并做了回归。

## Slice 9 改了什么

- **修布局：三栏各自独立滚动**（`frontend/src/index.css`）：根因是 `.app` 是 `min-height:100vh` 不是 `height:100vh`——内容一长整个页面就比屏幕高，浏览器出现整页滚动条，`.main`、`.sidebar`、`.activity` 的内部滚动根本触发不了，输入框也被顶走。修法：`html, body` 加 `height:100%; overflow:hidden`，`.app` 改成 `height:100vh; overflow:hidden`，高度锁死在一屏；`.layout` 保持 `flex:1; min-height:0`，三列高度都被锁住；左边 `.sidebar`、中间 `.main`、右边 `.activity` 各自 `overflow-y:auto`，自己滚自己的，互不影响；中间聊天区内部再分上面 `.chat-scroll` 自己滚、下面 `.composer`（`flex-shrink:0`）固定在底部；单次诊断的 `.run-scroll` 也一样处理。验证：20 条以上长对话里，滚中间聊天时右侧活动面板不动、一直能看到工具卡片，滚右侧时中间不动，输入框一直固定在底部。
- **执行器改通用 Patch**（`internal/agent/agent.go`、`internal/repair/repair.go`、`internal/store/store.go`、`internal/kube/client.go`、`internal/run/worker.go`）：`SuggestedRepair` 不再是 namespace/deployment/container/field/oldValue/newValue 这种只认内存的形状，改成通用 Patch 列表 `Patches []PatchOperation`，每条是 `{op, path, value}`，op 只支持 `replace`（不做 add/remove，避免删字段），path 例如 `/spec/template/spec/containers/0/image`、`/spec/template/spec/containers/0/resources/limits/memory`、`/spec/replicas`，value 例如 `"nginx:1.25"`、`"256Mi"`、`3`。校验改成 `ValidatePatch`（只按字段分，不按故障分）+ `ValidatePatchWithCurrent`（现状相关：容器下标越界、内存倍数/上限）：只允许改 Deployment，白名单 path 只有副本数（1-10）、容器镜像（合法镜像名）、容器内存/CPU 的 limit/request；黑名单明确拒绝 hostNetwork、hostPID、privileged、metadata/name、namespace、command、args；系统命名空间拒绝；内存新值大于旧值、不超过旧值 4 倍、不超过 4Gi。执行改成 `kube.RepairClient.PatchDeployment`，直接用 client-go 的 Patch API（JSONPatchType）把模型给的 Patches 列表原样发出去，执行前先 dry-run，Patch 前重读一次核对 resourceVersion 没变（JSONPatch 体里没有放 resourceVersion 的位置，这一步和原来把 resourceVersion 写进 strategic merge patch 的 metadata 是同一层保证），执行后逐条读回确认（镜像读 image、内存读 limits.memory、副本数读 replicas，不一致就标记执行失败）。rollout 验证保持原来的逻辑（observedGeneration、updated/available replicas、新 Pod Ready、重启次数不增长），没改。Worker 的 `maybeCreateProposal` 改成通用：按建议卡的 Patches 生成提案，容器下标按容器名重算，改前值逐条按现状读，第一个 Patch 的改前/改后值存进 OldValue/NewValue 做展示；`change_requests` 表加 `patches_json` 列，`ChangeRequest` 加 `Patches` 字段。前端 `RepairSuggestionCard` 和审批卡改成显示 Patch 列表（字段中文名 + path + 新值，旧值读不到只显示新值，不编旧值）。
- **向后兼容**（不删旧逻辑）：`SuggestedRepair` 里旧的 Field/OldValue/NewValue 字段保留，`Normalize()` / `ToPatches()` 会把旧的 memory-only 卡转成一条内存 limit 的 Patch；`repair.ValidatePatchSpec` 保留，内部转成通用 Patch 再校验；`store` 读老提案（patches_json 为空）时从 oldValue/newValue 反推一条内存 Patch；`kube.PatchDeploymentMemoryLimit` 保留，老测试照常用。`go run ./cmd/eval` 仍 3/3 通过。
- **多轮追问选镜像**（`internal/chat/real.go`、`internal/chat/stub.go`）：模型问“你想换成哪个镜像？是 nginx:1.25 还是 nginx:latest？”之后，用户回“换成 nginx:1.25”，下一轮模型把用户的回答转成结构化 Patch，而不是再问一遍。多轮记忆靠最近 10 条历史（已经有了）：提示词加了一句“用户上一轮已经明确说了新值时，直接用那个值生成结构化 Patch，不要再问一遍”。Stub 路径也支持：消息里有镜像名 + 换/改/升级，或者上一轮刚问过选哪个镜像、这轮只回了一个镜像名，就直接查当前镜像、给 Patch 卡。mock 模型服务两轮对话测试已覆盖。
- **主动变更走同一套 Patch**（`internal/chat/real.go`、`internal/chat/stub.go`）：用户说“帮我把 demo-api 的镜像换成 nginx:1.26”，Pod 是健康的，不用诊断，直接查一下当前镜像是什么，确认一下，然后给一张 Patch 建议卡，点了“创建可审批的修复任务”，审批后执行，再验证 rollout。提示词加了一句“用户明确说了要改成什么值时，不用先做故障诊断：先查当前值确认一下，再直接给出 Patch 建议”。mock 模型 + Stub 两路测试已覆盖（主动变更只调 get_deployment，不调 list_problem_pods、get_pod、get_events 这些诊断工具）。
- **提示词通用化**（`internal/chat/real.go`、`internal/model/model.go`）：对话提示词第 6、7 条和单次诊断提示词第 5、7 条不再只写内存，改成“用 patches 列表描述要改的字段”；`submit_diagnosis` 的 suggestedRepair 工具定义加了 `patches` 数组字段，旧的 field/oldValue/newValue 保留做兼容。提示词和工具描述里不写“不要调 xxx”这种禁止句，只描述每个工具返回什么、Patch 白名单里有什么，让模型自己判断。
- **Stub 的镜像建议**（`internal/model/model.go`、`internal/chat/chat.go`，测试路径专用）：Stub 的 imagepull 场景现在也给一张镜像 Patch 卡（demo-api:bad-tag → demo-api:latest），和 OOM 的内存卡走同一套通用 Patch；`synthesizeRepair` 也支持镜像卡。注意：这两处都是桩模型测试路径的确定性规则，真模型路径不走这里；真集群上的坏镜像 tag 各不相同，新镜像只能由模型按证据和用户回答决定，规则合成猜不出一个正确的 tag。另外修了一个相关 bug：`synthesizeRepair` 以前把 runbook 检索片段也当成一手证据，查 ImagePullBackOff 时 OOM 手册里的 "out of memory" 会把镜像案误判成内存案，现在检索片段只看摘要、不参与信号判断。
- **回归**：`go vet ./...` 通过，`go test ./...` 全部通过（含新增的通用 Patch 白名单、通用 Patch 执行（镜像/内存/副本数/多 Patch）、多轮追问、主动变更、Stub 镜像卡、提案 Patch 持久化、旧卡向后兼容测试），`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、转正式诊断后审批执行验证全部回归通过。

## 通用 Patch 格式和白名单

建议卡 / 提案里的 Patch 列表：

```json
{"patches": [{"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "nginx:1.25"}]}
```

| path | value | 说明 |
| --- | --- | --- |
| `/spec/replicas` | 1–10 的整数 | 改副本数 |
| `/spec/template/spec/containers/{i}/image` | 合法镜像名，如 `nginx:1.25` | 改镜像 |
| `/spec/template/spec/containers/{i}/resources/limits/memory` | Mi/Gi，如 `256Mi` | 新值 > 旧值、≤ 旧值 4 倍、≤ 4Gi |
| `/spec/template/spec/containers/{i}/resources/requests/memory` | Mi/Gi | 同上（旧值为空时只检查上限） |
| `/spec/template/spec/containers/{i}/resources/limits/cpu` | 如 `500m`、`1` | 合法 CPU 数量 |
| `/spec/template/spec/containers/{i}/resources/requests/cpu` | 如 `500m`、`1` | 合法 CPU 数量 |

op 只支持 `replace`。明确拒绝：hostNetwork、hostPID、privileged、metadata/name、metadata/namespace、command、args，以及 kube-system / kube-public / kube-node-lease。容器下标按容器名重算，模型给错下标也不会改错容器。

多轮追问和主动变更的用法（对话模式）：

```bash
# 模型问选哪个镜像，用户选了之后继续审批执行
# 第一轮：问“demo 那个一直拉不到镜像的是怎么回事”
#   → 模型问“你想换成哪个镜像？是 nginx:1.25 还是 nginx:latest？”
# 第二轮：回“换成 nginx:1.25”
#   → 模型返回一张 image Patch 建议卡，点“创建可审批的修复任务”走审批执行

# 主动变更：健康 Pod 换镜像版本，不用先诊断
# 问“把 demo-api 的镜像换成 nginx:1.26”
#   → Agent 先查当前镜像，再直接给一张 Patch 建议卡
```

## Slice 9 未实现（仍未做）

- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控
- op 只支持 replace：add/remove 以后需要时再加，现在不做，避免删字段
- command/args、hostNetwork、privileged 这些字段先禁掉，以后用户明确要求时再放开

Slice 8 修的是用户在 Mac 真机上反馈的“当前状态怎么样”8 步用完报错的问题（2026-11-20）：对话里问一句总览，页面中间一片空白，下面只有一句粉色报错“对话已达最大工具步数（8 步），请换个更具体的问题再问”。核心是三件事：**步数是预算不是决策**（MaxChatSteps 从 8 调到 12，步数用完返回已查到的半成品，不再直接报错）、**工具描述只描述、不禁止**（讲清每个工具返回什么、覆盖多大范围，删掉“不要调 xxx”这种替模型做决策的禁止句）、**提示词改成先给总览、等追问再深挖**（一轮里不要既看全貌又深挖）。另外加了一个轻量配置中心：`config.yaml` + 环境变量覆盖 + `GET/POST /api/config` 热加载，前端“设置”页能改，不用重启。

Slice 1/2/3/4/5/6/7 已有的东西没有重写：SQLite 表、HTTP 接口、Worker 状态机、Agent 循环的校验和恢复、fake 客户端、桩模型、真模型适配器、FTS5 检索、条件 Patch 与 rollout 验证、对话会话、发现工具、React 前端，全部保留并做了回归。

## Slice 8 改了什么

- **MaxChatSteps 从 8 改成 12，步数用完返回半成品，不再直接报错**（`internal/chat/chat.go`、`internal/chat/real.go`）：根因不是模型不会决策，是预算太小 + 预算用完时后端直接 `return error`，RunMessage 把错误丢回前端，用户看到的是空白加一句“请换个更具体的问题再问”——等于把预算用完的锅推给用户，问题本身（“当前状态怎么样”）一点错没有。修法两层：预算调到 12（对话专用，单次诊断的 8 步不动）；`runReal` 循环跑完 12 步还没收敛时，把已经查到的工具结果（`outputs` 的摘要 + `structured` 的规范 key）整理成半成品回复返回，文字写“已经查到了这些，但步数用完了，还没来得及总结：”然后列出已查到的总览和问题 Pod，再问“你想深入查哪个命名空间或 Pod？”。RunMessage 把半成品正常落盘成一条助手消息，前端按 `structured` 里的 `overview` / `problemPods` 正常渲染卡片和表格，刷新后也还在。MaxChatSteps 是预算不是决策：调大它不干预模型怎么选工具，只是允许一轮里查得更多。
- **工具描述补全，只描述、不禁止**（`internal/model/model.go` 的 `toolDefs`）：每个工具的 description 现在讲清返回什么、覆盖多大范围，例如 `get_cluster_overview` 一次返回全集群汇总（节点数、命名空间数、Deployment 数、Pod 数、有问题 Pod 数），`list_problem_pods` 不带 namespace 时一次返回全部命名空间的有问题 Pod、带时只返回那个命名空间的，`list_namespaces` 只返回名字列表、不返回 Pod 状态，`list_deployments` 返回某个命名空间的 Deployment 列表、不返回 Pod 详情。描述里不写“不要调 xxx”“不再需要调 xxx”这种禁止句——该调哪个由模型按返回内容自己判断，写死禁止规则是替模型做决策。新增测试 `TestToolDescriptionsDescribeOnly`，描述里出现禁止句就失败。
- **提示词改成“先给总览，等追问再深挖”**（`internal/chat/real.go` 的 `chatSystemPrompt`）：原来“先用发现工具看全貌、找问题，再用定向工具深挖”会鼓励模型一轮里既看全貌又深挖，浪费步数。现在改成：总览问题先查全集群状态、给出摘要就停下来，等用户追问某个命名空间或 Pod 时再深挖，不要在一轮里把所有命名空间、所有 Pod 都查完。没有写死“总览就是 2 个工具”——调几个工具由模型自己判断。指代消解、只读、诚实、修复建议必须结构化返回这几条原样保留。
- **配置中心（轻量）**（新增 `internal/config/config.go`、`config.yaml`，`internal/api/api.go`、`cmd/server/main.go`）：配置文件 `config.yaml` 放仓库根目录，字段有 `chat.maxSteps`（12）、`chat.maxHistoryMessages`（10）、`run.maxSteps`（8）、`run.timeoutSeconds`（60）、`model.timeoutSeconds`（30）、`model.temperature`（0.2）、`repair.maxMemoryMi`（4096）、`repair.maxRatio`（4）。启动时先读文件，没有文件用默认值；环境变量可以覆盖（`CHAT_MAX_STEPS`、`CHAT_MAX_HISTORY_MESSAGES`、`RUN_MAX_STEPS`、`RUN_TIMEOUT_SECONDS`、`MODEL_TIMEOUT_SECONDS`、`MODEL_TEMPERATURE`、`REPAIR_MAX_MEMORY_MI`、`REPAIR_MAX_RATIO`），环境变量优先于文件。`GET /api/config` 读当前配置，`POST /api/config` 改配置：校验范围（非法值例如 `maxSteps=0`、`temperature=2` 返回 400，不保存、不生效），保存到 `config.yaml`，热加载不用重启。`MODEL_API_KEY` 绝不进配置中心：配置结构里没有这个字段，GET 不返回，前端不显示、不让改，Key 只从环境变量读。`internal/chat` 的步数/历史数、`internal/store` 的任务创建默认值、`internal/repair` 的上限（`MaxMemoryMi` / `MaxRatio` 从常量改成变量）、真模型的超时/温度，启动和热更新时都从配置中心读。
- **前端“设置”页**（`frontend/src/App.tsx`、`frontend/src/api.ts`、`frontend/src/types.ts`、`frontend/src/index.css`）：header 旁边加了一个“设置”按钮，点开弹窗，8 个参数各一个输入框、带一句中文说明（例如“对话最大工具步数：模型一轮最多调几次工具，调大可以查得更全，但更慢更贵”），点“保存”调 `POST /api/config`，保存后显示“已保存，立即生效，不用重启”。半成品回复不需要前端特殊处理：它就是一条普通的助手消息，`StructuredView` 按 `overview` / `problemPods` 正常渲染卡片和表格；旧的“请换个更具体的问题再问”那句报错后端已经不再返回，前端也不再显示它。
- **回归**：`go vet ./...` 通过，`go test ./...` 全部通过（含新增的配置加载/校验/热更新、步数用完返回半成品、配置中心 GET/POST、工具描述无禁止句、修复上限跟配置五个测试），`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、转正式诊断后审批执行验证全部回归通过。

Slice 1/2/3/4/5/6/7 已有的东西没有重写：SQLite 表、HTTP 接口、Worker 状态机、Agent 循环的校验和恢复、fake 客户端、桩模型、真模型适配器、FTS5 检索、条件 Patch 与 rollout 验证、对话会话、发现工具、React 前端，全部保留并做了回归。

## 配置中心

`config.yaml`（仓库根目录）+ 环境变量覆盖 + `GET/POST /api/config` 热加载。启动时先读文件，没有文件用默认值；环境变量优先于文件；前端“设置”页改完点保存，立即生效，不用重启。

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `chat.maxSteps` | 12 | 对话最大工具步数：一轮对话里模型最多调几次工具。预算，不是决策——调大不干预模型怎么选工具，只是允许查得更多；步数用完返回半成品，不报错 |
| `chat.maxHistoryMessages` | 10 | 每次调用模型时带上的历史消息数，更早的消息数据库里全量保留 |
| `run.maxSteps` | 8 | 单次诊断（/api/runs）最大工具步数，和对话的 12 步分开 |
| `run.timeoutSeconds` | 60 | 单次诊断整体超时秒数，超时后用已有证据收敛 |
| `model.timeoutSeconds` | 30 | 每次模型 API 调用的超时秒数 |
| `model.temperature` | 0.2 | 模型温度，0–1，越低越按证据说话 |
| `repair.maxMemoryMi` | 4096 | 修复建议的新值上限（MiB），即 4Gi |
| `repair.maxRatio` | 4 | 修复新值不超过旧值的倍数，两个上限取更严的那一个 |

环境变量覆盖（部署时用，不用改文件）：`CHAT_MAX_STEPS`、`CHAT_MAX_HISTORY_MESSAGES`、`RUN_MAX_STEPS`、`RUN_TIMEOUT_SECONDS`、`MODEL_TIMEOUT_SECONDS`、`MODEL_TEMPERATURE`、`REPAIR_MAX_MEMORY_MI`、`REPAIR_MAX_RATIO`。配置文件路径可用 `CONFIG_PATH` 指定，默认找 `config.yaml`（依次试当前目录、`../config.yaml`、`../../config.yaml`）。

`MODEL_API_KEY` 绝不进配置中心：不写进 `config.yaml`、不出现在 `GET /api/config` 的返回里、前端不显示、不让改，只从环境变量读。

注意：通过前端“设置”页保存后，`config.yaml` 里的注释会被重写掉（YAML 序列化不保留注释），值本身不变；想保留注释就直接改文件后重启，或改完文件再用前端改一次值。

```bash
# 读当前配置
curl -s http://localhost:8080/api/config

# 改一个值（对话步数改成 16），保存后立即生效，不用重启
curl -s -X POST http://localhost:8080/api/config \
  -H 'Content-Type: application/json' \
  -d '{"chat":{"maxSteps":16,"maxHistoryMessages":10},"run":{"maxSteps":8,"timeoutSeconds":60},"model":{"timeoutSeconds":30,"temperature":0.2},"repair":{"maxMemoryMi":4096,"maxRatio":4}}'

# 非法值会被拒绝（400），旧配置继续生效
curl -s -X POST http://localhost:8080/api/config \
  -H 'Content-Type: application/json' \
  -d '{"chat":{"maxSteps":0,"maxHistoryMessages":10},"run":{"maxSteps":8,"timeoutSeconds":60},"model":{"timeoutSeconds":30,"temperature":0.2},"repair":{"maxMemoryMi":4096,"maxRatio":4}}'
```

## Slice 7 改了什么

Slice 7 修的是用户在 Mac 真机（kind + 真模型）上反馈的 6 个问题，外加一个全新的故障案例 ImagePullBackOff，供用户测试 Agent 的通用发现能力。核心是三件事：**白屏不再发生**（后端报告空数组归一化 + 前端判空 + 错误边界）、**聊天不再是纯文本**（后端真模型路径存规范 key + 前端轻量 Markdown 渲染）、**正式诊断的按钮和位置说清楚**（只在有建议卡时显示，按时间顺序插进消息流）。

- **修白屏（最优先）**（`internal/agent/agent.go`、`internal/model/model.go`、`internal/chat/real.go`、`frontend/src/components.tsx`、`frontend/src/types.ts`）：根因是真模型返回的 JSON 缺字段时，Go 解出来是 nil，再序列化成 JSON 就是 null，前端读 `report.facts.length` 直接抛错，整个 React 应用白屏。修法三层：后端 `Report.Normalize()` 把 facts、evidenceIds、recommendations、openQuestions 缺失时补成空数组，`ValidateReport` 和 `parseDiagnosisArgs` 都会调它，序列化出来是 `[]` 而不是 null；前端 `normalizeReport` 再兜一层，历史库里旧的 null 报告也能渲染，`ReportView`、`OverviewCards`、`PodTable`、`DeploymentTable`、`buildBlocks` 里所有 `.length`、`.map` 的地方都先判空；再加一个 React `ErrorBoundary` 包住聊天消息区和正式诊断卡片，某个卡片渲染出错时只显示“这个卡片渲染失败了，请刷新重试”，不让整个页面白屏。新增测试 `TestReportNormalizeEmptyArrays` 和 `TestValidateReportNormalizes`。
- **正式诊断按钮说清楚 + 只在有建议时显示 + 快照按时间顺序**（`frontend/src/App.tsx`、`frontend/src/components.tsx`）：按钮文案从“转成正式诊断”改成“创建可审批的修复任务”，下面加一句小字解释：“生成一份带提案的诊断任务，人工审批后才会执行，不会直接改集群。”输入框旁边的按钮删掉了——没有建议卡时点它没有意义，入口只在修复建议卡上。快照卡片不再固定在聊天底部：转换时记下最后一条消息的 id 作为锚点，审批卡插在那条消息后面按时间顺序出现，之后再发的新问题会出现在它下面。**聊天和正式诊断的区别**：聊天是探索，只读不改，没有审批，没有执行记录；正式诊断是一个任务，有 runId、有状态、有步骤、有报告、有提案、有审批、有执行和验证，全都存在数据库里，能审计，能重启后恢复。按钮存在是因为聊天不能直接动手改集群，必须先生成一个正式任务走人工审批——这是安全边界，不是多余的一步。
- **runbooks 不是特例**（不用改代码，界面和文档写清）：`runbooks/*.md` 是 RAG 知识库，8 篇 Markdown，SQLite FTS5 索引，模型通过 `search_runbooks` 工具自己搜，不是代码里 `if OOM 就返回某篇`。评估的 3 个用例是测试夹具，不是产品逻辑。界面上现在能看到引用了哪篇手册：`search_runbooks` 返回的片段带标题、来源文件和关键句，前端按 `runbooks` 结果块渲染成“参考手册”，不再只显示 ev-1、ev-2 这种编号。加一篇新的 `.md` 文件重启就进索引，模型就能搜到。
- **后续追问不再纯文本 + 聊天记录 Markdown 渲染**（`internal/chat/real.go`、`frontend/src/components.tsx`、`frontend/src/types.ts`）：根因是真模型路径只把工具结果存进 `structured[工具名]`（例如 `structured["get_pod"]`），前端 `buildBlocks` 只认规范 key，工具名当 key 的结果会被过滤掉，聊天里就只剩文字、没有卡片，刷新后也一样。现在每个工具的结果同时写一份规范 key：`get_cluster_overview` → `overview`、`list_problem_pods` → `problemPods`、`get_pod` → `pod`、`get_deployment` → `deployment`、`list_deployments` → `deployments`、`get_events` → `events`、`search_runbooks` → `runbooks`，工具名那份留着做追溯，两份都落盘，刷新后卡片还在。前端加了一个手写的轻量 Markdown 渲染（不引入重型库，100 行以内）：助手消息支持 `**加粗**`、`` `代码` ``、`- 无序列表`、`1. 有序列表`和分段，渲染失败时 fallback 回纯文本。新增测试 `TestRealCanonicalKeys`（mock 模型服务按顺序返回两个工具调用再给纯文本，验证 Structured 里有 overview 和 problemPods，刷新后还在）。
- **输入框固定底部**（`frontend/src/App.tsx`、`frontend/src/index.css`）：以前输入框在能滚动的 `.main` 里面，消息一长就被顶出去了。现在 `.main` 本身不再滚动，里面分两块：上面 `.chat-scroll`（聊天）/ `.run-scroll`（单次诊断）自己滚动，下面 `.composer` 固定在底部不跟着滚。只改聊天模式，单次诊断的表单不动。
- **新案例：ImagePullBackOff**（`scripts/kind/demo-bad-image.yaml`、`scripts/kind/setup-bad-image.sh`，见下文“新案例”一节）：namespace demo，Deployment demo-bad-image，镜像 `nginx:9.9.9-nonexistent`，Pod 卡在 ImagePullBackOff。代码里没有任何针对这个案例的特殊处理：Go 代码里没有 `demo-bad-image` 这个字符串，没有为它加 runbook（已有的 `runbooks/imagepull.md` 已够用），fake 客户端里也没有预置它。这个案例只在真集群上测，用来验证 Agent 能不能靠通用工具自己发现问题。
- **回归**：`go test ./...` 全部通过（含新增的报告归一化、真模型规范 key 两个测试），`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、转正式诊断后审批执行验证全部回归通过。

## 新案例：ImagePullBackOff（demo-bad-image）

这是 Slice 7 新增的第二个故障案例，用来测试 Agent 的通用发现能力——它能不能发现一个代码里完全没特殊处理过的问题。

```bash
# 部署（Mac 上，kind 集群已建好）
kubectl apply -f scripts/kind/demo-bad-image.yaml
# 或一键脚本（会等 Pod 进入 ImagePullBackOff 再提示就绪）
bash scripts/kind/setup-bad-image.sh

# 手动验证
kubectl get pods -n demo
kubectl describe pod -n demo -l app=demo-bad-image

# 清理
kubectl delete -f scripts/kind/demo-bad-image.yaml
```

案例内容：namespace demo，Deployment demo-bad-image，1 个副本，镜像 `nginx:9.9.9-nonexistent`（不存在的 tag），容器名 demo-bad-image，资源 requests 64Mi/100m、limits 128Mi/500m（和 demo-api 一样，不搞特殊值）。没有内存泄漏逻辑，不是 OOM 场景。

预期现象：Pod 卡在 ImagePullBackOff 或 ErrImagePull，事件里有 `Failed to pull image`，日志为空。

Agent 应该发现什么：在对话里问“demo 那个一直拉不到镜像的是怎么回事”，Agent 应该通过 `list_problem_pods`、`get_pod`、`get_events`、`search_runbooks`（搜 ImagePullBackOff）自己发现镜像 tag 不存在，给出文字建议“镜像 tag 不存在，改成存在的 tag，例如 nginx:1.25”。

Agent 不应该给什么：**不应该给出一张内存修复卡**——不是 OOM，调内存 limit 对拉不到镜像没有任何帮助。如果 Agent 给了内存修复卡，就是 bug，请把当时的对话截图发回来。

代码里没有任何针对这个案例的特殊处理：Go 代码里搜不到 `demo-bad-image`，没有为它加 runbook，没有为它加 fake 数据。`demo-api` 的 OOM 案例仍在，两个案例可以并存（都在 demo 命名空间，Pod 名不同）。

## Slice 6 改了什么

- **工具时间线移到右侧活动面板**（`frontend/src/App.tsx`、`frontend/src/components.tsx`）：布局改成左侧会话列表、中间聊天、右侧“Agent 活动”面板。右侧不再放诊断报告、审批卡、修复验证，只放工具执行过程：每一步一张卡片，显示工具名、参数摘要、状态（进行中带转圈高亮 / 成功打勾 / 失败打叉标红）、结果摘要、耗时，卡片可点开展开看详情。聊天里不再贴完整时间线，只留一行摘要，例如“查了 6 个工具 · 总耗时 12ms”，点一下展开看完整时间线。单次诊断也一样：任务区只留一行摘要，完整时间线在右侧面板。
- **工具调用带耗时**（`internal/chat/`）：对话的 `ToolCallRecord` 新增 `durationMs` 字段，调工具时记开始和结束时间算出来再返回；单次诊断的 step 里本来就有 `durationMs`，直接用。工具极快导致耗时为 0 时前端显示“—”，不编一个数字。字段不用 omitempty，0 本身就是一个明确的值。
- **滚动控制**（`frontend/src/App.tsx`）：聊天只有用户已经在最底下时才跟着滚到底，否则显示“↓ 有新消息”按钮，不把正在往上翻历史的用户拽下来。
- **详情结果全部回聊天区，按类型渲染**（`frontend/src/types.ts` 的 `buildBlocks`、`frontend/src/components.tsx` 的 `ResultBlockView`）：后端返回的结构化结果先按 key 映射成带类型的结果块再渲染，类型有 overview（集群总览卡片）、podList（Pod 表格）、deploymentList（Deployment 表格）、events（事件时间线）、report（诊断报告卡）、repairSuggestion（修复建议卡）、text（纯文本）。工具名原样当 key 的原始结果（如 get_pod）是 canonical key 的重复，不再单独成块，避免同一份数据出现两次。不知道的类型不渲染，不白屏。每个结果块默认折叠一部分：Pod 表格先显示前 10 行，点开看全部；事件先显示前 5 条，点开看全部；修复建议卡只显示一行摘要，点开看详情。审批卡、修复执行和验证结果也在聊天区作为结果块出现：点了“转成正式诊断”后留在对话模式，审批卡直接出现在聊天里，不用再去右侧找。
- **修复决策只走模型单一路**（`internal/chat/real.go`、`internal/run/worker.go`、`internal/model/model.go`）：真模型路径去掉 `synthesizeRepair` 兜底。模型给了最终回答但没有结构化修复建议时，不再由后端按 OOM 证据合成一张卡，而是再问模型一次，让它用 `submit_diagnosis` 把 `suggestedRepair` 补回来；补不回来就老实说还没法下结论、需要更多证据，不要编一张卡。正式诊断的 Worker 也一样：模型报告里没给新值就不生成提案，不再按旧值翻倍兜底。系统提示词（对话和单次诊断两处）都加了一句：用户明确说了增加量（例如“增加 128Mi”、“调到 256Mi”）时，模型按用户说的算新值，不要默认翻倍；解析用户的话是模型的事，后端不做正则解析，只校验模型算出来的数。桩模型（Stub）的 `synthesizeRepair` 调用留着，但明确标注只是测试路径的确定性规则，不是产品逻辑；离线评估（`go run ./cmd/eval`）走桩模型，仍 3/3 通过。
- **修复建议卡丢失 bug 修复**（`internal/chat/chat.go` 的 `RunMessage`）：根因是保存助手消息时只存了 ToolCalls 和 Structured，没存 SuggestedRepair；runStub 返回的建议只在 Reply 顶层，前端重新拉会话时就丢了，按钮也没了。现在统一在 `RunMessage` 里做：只要本轮给了建议，就写进 `Structured["suggestedRepair"]`，stub 和 real 两条路都走同一个保存逻辑。runStub 里也同步把建议写进 Structured，两边一致。前端的“转成正式诊断”按钮只要有建议卡就一直显示，不只在最新一条消息上才显示，翻历史、刷新后按钮还在。安全流程不变：聊天里不给直接“执行”按钮，修复还是要走“转成正式诊断 → 人工审批 → 执行验证”那条线，不绕过审批。
- **回归**：`go test ./...` 全部通过（含新增的建议卡持久化、工具耗时、真模型无合成卡、Worker 缺新值不生成提案四个测试），`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、转正式诊断后审批执行验证全部回归通过。

## Slice 5 改了什么

- **完整 React 前端**（`frontend/`，Vite + React + TypeScript，不引入 UI 框架）：左侧会话列表（新建会话、会话历史），中间聊天区和单次诊断，右侧详情区（当前任务的报告、审批卡、修复验证）。聊天区里助手消息内嵌工具执行时间线：每一步显示工具名、参数摘要、状态（成功 / 失败）、结果摘要，正在执行的工具带转圈和“正在执行”提示。结构化结果渲染成卡片和表格：集群总览是数字卡片，Pod 列表和 Deployment 列表是表格（系统行带“系统”徽章），事件是时间线，修复建议卡、审批卡、执行/验证结果分开显示。单次诊断页保留 Slice 3 的全部功能（提交、轮询时间线、报告、审批、执行验证），只是用 React 重写。空状态和错误状态都有文案，不会白屏。
- **工具执行实时显示用轮询，不用 WebSocket**：对话的 POST 在服务端跑整轮，期间 tool 消息会陆续落盘，前端每 800ms 轮询一次会话，就能实时看到正在执行哪个工具；单次诊断每 900ms 轮询 `GET /api/runs/{id}/events?after=N` 追加时间线，同时刷新任务快照。轮询够用，后端不用为实时显示改协议。
- **前后端同仓库、单二进制部署**：React 源码在 `frontend/`，构建产物输出到 `web/dist/`，Go 用 `go:embed` 把 dist 打进二进制。交付 zip 里自带构建好的 dist，解压后不用装 Node，直接 `go run ./cmd/server` 就能看到 React 页；想改前端时再按 `frontend/README.md` 装 Node 重建。`make frontend` 是装依赖 + 构建，`make run` 是直接启动（dist 已包含），`make run-frontend` 是先构建前端再启动。
- **系统命名空间读放开、写挡住**：去掉发现工具（`internal/run/tools.go`）对 kube-system / kube-public / kube-node-lease 的过滤，`list_namespaces`、`list_deployments`、`list_problem_pods`、`get_cluster_overview` 都能返回系统命名空间的结果，行里带 `isSystem: true` 标记，总览里单独列出系统命名空间数和系统有问题 Pod 数；对话的系统提示词改成“可以查系统命名空间做诊断，但不要把它当成用户业务下结论，不要给系统组件出修复建议”；`internal/agent.ValidateToolCall` 和 `POST /api/runs` 的创建校验也不再拒绝系统命名空间（诊断是只读）。写操作继续严格挡住：`internal/repair.ValidatePatchSpec` 对系统命名空间的拒绝原样保留，RBAC 清单不动。
- **回归**：`go test ./...` 全部通过，`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、修复建议卡 128Mi→256Mi、对话只读、转正式诊断后审批执行验证全部回归通过。

## Slice 4 改了什么

- **通用 Agent**（`internal/agent/`、`internal/model/`、`internal/run/worker.go`）：报告里新增可选的 `suggestedRepair` 字段（目标 namespace / Deployment / 容器、字段、旧值、新值、理由），字段目前只支持 `memoryLimit`，别的字段会被校验层清掉。Stub 的修复建议从已查到的 Deployment 结果里算出来（容器名、旧值来自证据，新值是旧值翻倍封顶 4Gi），不再写死 demo-api / 128Mi；真模型的 `submit_diagnosis` 也加了 `suggestedRepair` 字段，系统提示词里明确“不要预设故障类型，按证据自己判断”。
- **通用修复白名单**（`internal/repair.ValidatePatchSpec`）：任意 namespace / Deployment / 容器都可以提修复建议，但只允许改内存 limit，值必须是 Mi / Gi 结尾，新值必须大于旧值、不超过旧值的 4 倍、不超过 4Gi，系统命名空间一律拒绝。旧值和集群现状是否一致仍由执行前校验重新读目标时核对（旧值不匹配标为过期，不 Patch）。
- **通用提案生成**（`internal/run/worker.go`）：Worker 不再看原因里有没有 OOM，而是看报告里有没有 `suggestedRepair`；容器名没给时读 Deployment 的第一个容器，旧值以集群现状为准（建议里的旧值和现状不一致就放弃），新值必须由模型给，没给就不生成提案（Slice 6 去掉了以前“新值缺失时按旧值翻倍”的兜底，修复决策只能由模型做）。校验不通过就不生成提案，诊断报告本身仍然完整。
- **KUBE_MODE 通用化**（`cmd/server/main.go`）：`fake | kubeconfig`，`kind` 是 `kubeconfig` 的别名，做兼容保留。kind、k3s、云厂商集群都读 kubeconfig，不再为每种集群写一套客户端；k3s 的 kubeconfig 在 `/etc/rancher/k3s/k3s.yaml`，用 `KUBECONFIG` 指过去即可。
- **发现工具**（`internal/run/tools.go`、`internal/kube/client.go`）：新增 4 个只读工具，注册进 Agent 工具表，模型在对话里能自主调用：`list_namespaces`、`list_deployments`（namespace 为空查全部）、`list_problem_pods`（非 Running、未 Ready、重启过、有等待原因）、`get_cluster_overview`（节点数、namespace 数、Deployment 数、Pod 数、有问题 Pod 数）。返回的都是结构化 JSON，前端渲染成表格和卡片。注意：Slice 4 时系统命名空间的结果会被过滤掉，Slice 5 已改成读放开、写挡住，系统行带 `isSystem: true` 标记，见上文 Slice 5。
- **对话会话**（`internal/chat/`、`internal/store/`）：SQLite 新增 `conversations` 和 `messages` 两张表（role 分 user / assistant / tool）。新 API：`POST /api/conversations`、`GET /api/conversations`、`GET /api/conversations/{id}`、`POST /api/conversations/{id}/messages`、`POST /api/conversations/{id}/convert`。每次调用模型带最近 10 条消息，超出截断，数据库里全量保留；TODO 是以后对 10 条之前的历史做摘要，现在不做假的摘要。指代消解靠历史实现：系统提示词写清“这个 ns / 刚才那个 Pod 要按最近提到的目标理解，理解不了就主动问，不要猜”。
- **对话只读**：对话里模型只能调只读工具，不能直接 Patch；需要修复时返回结构化修复建议卡，不生成 `change_requests`。用户点“转成正式诊断”后，才走现有的 `/api/runs` 流程，生成提案、走人工审批、再执行和验证。
- **Runbook 扩到 8 篇**：新增 CrashLoopBackOff、Pending 调度失败、Service 无 Endpoints、ConfigMap/Secret 缺失、资源配额不足 5 篇，每篇都有中文关键词和别名，FTS5 索引能命中；原有 3 篇没删，`go run ./cmd/eval` 仍 3/3 通过。

## Slice 3 改了什么

- **修复执行**（`internal/repair/`）：Worker 领取已批准的提案后，先重新读取目标，核对 UID、容器名、变更前旧值，再 dry-run，再用本次读取的 resourceVersion 做条件 Patch，只改指定 Deployment、指定容器的内存 limit 这一个字段，值只允许是提案里的新值。写入后读回确认，写入超时或中断时先读回目标再下结论，归因不明确记 unknown，不谎报成功。前提变了（UID 变了、旧值变了、容器没了）标为过期，不执行；当前值已是新值时直接记为已执行，不重复 Patch。
- **白名单**（`internal/repair.ValidatePatchSpec`）：只允许 demo/demo-api/demo-api 这一组目标，内存值必须是 Mi 结尾，新旧值不能相同，其他一律拒绝。RBAC 只能限到资源和动词，改哪个字段是代码白名单在管，两层都要有。
- **写权限分离**：诊断走 `kube.KubeClient`（只读），修复走新增的 `kube.RepairClient`（PatchDeploymentMemoryLimit 等），Fake 和 Real 各实现一份。本地 kind 演示仍用同一个 kubeconfig，但对象是分开建的；生产上把 `REPAIR_KUBECONFIG` 指向写身份专用的 kubeconfig 即可，修复代码不用改。
- **rollout 验证**（`internal/repair/`）：独立检查 observedGeneration 追上 generation、updatedReplicas 和 availableReplicas 达标、新 Pod Ready、验证窗口内重启次数没有继续增长。新 Pod 按 pod-template-hash 区分，不把旧 Pod 的 Ready 当成新 Pod 的。超时 120 秒（默认），到点标 timeout，不一直循环；连续两轮 Ready 才算通过，避免只看一眼就放行。执行和验证结果分开存、分开展示，写入成功但验证失败时前端能分别看到。
- **Fake 模拟 rollout**：fake clientset 没有 Deployment 控制器，Patch 后不会自动建新 Pod，所以 Fake 的 Patch 在写完后模拟控制器行为：generation +1、status 置为已更新且可用、建带新 hash 的 ReplicaSet 和一个 Ready 的新 Pod。只在 Fake 里模拟，Real 不走这条路。
- **Worker 接入**：诊断没有任务时，Worker 轮询已批准的提案并执行验证。诊断和修复状态分开，诊断报告能独立于修复状态展示。等待审批的提案一直存在数据库里，重启后仍能领取；执行中途崩掉的提案重启后改回待执行，重跑是安全的（执行前校验先看当前值）。
- **提案生成放宽**：原来只在 high 置信度时生成提案，真模型在 kind 上跑 OOM 常常只给 medium（日志为空、没有实时 metrics，它不敢说 high），但 OOM 本身是直接原因，调高 limit 是低风险、可逆的操作，而且还要经过人工审批，所以 high 或 medium 都生成，low 仍不生成。
- **前端静态页**（`web/index.html`，仍是静态页，不是 React）：加了审批卡（目标、变更前后值、相关证据，批准/拒绝按钮）和修复展示（执行结果和验证结果分开显示、rollout 状态、新 Pod、重启次数）。无构建步骤，Go 服务同源托管。
- **RBAC 清单**（`scripts/kind/rbac.yaml`）：只读身份和受写身份的参考清单，写身份只能 patch demo-api 这一个 Deployment（resourceNames 限定）。
- **修复了两个 Slice 2 遗留问题**：Agent 工具失败不回传给模型的问题在 Slice 2 已修，本次未改坏；另外本次发现并修了一个相关问题——提案生成时没有绑定 UID，执行前无法校验目标是否被重建，现在生成时绑定 UID，执行前核对。

## Slice 2 改了什么

- **真模型适配器**（`internal/model/`）：OpenAI 兼容的 chat completions + tools 协议，不写死厂商。模型返回的 tool_calls 映射成 `agent.ToolCall`，`submit_diagnosis` 映射成最终报告；非法工具名原样交给 Agent 的 `ValidateToolCall` 拒绝，不崩循环。超时、重试（指数退避，最多 3 次，只重试 429 / 5xx / 网络错误）、上下文截断（只传最近 6 步摘要，每条最多 500 字，全量证据仍在 SQLite 里）。
- **知识库升级**（`internal/knowledge/`）：`runbooks/*.md` 在启动时建成 SQLite FTS5 索引，`search_runbooks` 改用 FTS5 按相关性排序，返回片段和来源。注意一个坑：FTS5 的 unicode61 会把一整句中文当成一个长词，查“内存”查不到，所以表里多了一个 keywords 列，存空格分开的别名（OOM、内存、探针……），短词查询才能命中。FTS5 未启用、出错或查不到时回退到 Slice 1 的关键词检索。
- **离线评估扩充**（`cmd/eval/` + `testdata/cases/`）：除 OOM 外新增镜像拉取失败、探针失败两个用例，每种用例有自己的 fake 场景数据（Pod / Events / 日志）。输出每个用例的通过与否、命中率、证据引用准确性、工具步数、耗时汇总。`MODEL_MODE=real` 但无 Key 时跳过真模型评估并明确提示，不报错退出。
- **kind 真集群演示脚本**（`scripts/kind/`）：`demo-api.yaml` 用公开的 `python:3.12-slim` 镜像不断分配内存，真实触发 OOM，不需要自己构建镜像；`setup-demo.sh` 一键建 namespace、应用 Deployment、等到 OOMKilled 或重启次数达标，并打印手动验证命令。
- **Stub 扩展**：桩模型从只会 OOM 扩展到 OOM / 镜像 / 探针三类场景，仍是确定性剧本，只为让 eval 三个用例在 stub 模式下跑通。修了一个 Slice 2 发现的 bug：探针案曾被 search_runbooks 带回的镜像手册带偏，误判成镜像拉取；现在 Stub 先看 Pod / Events / 日志这些一手证据，再看检索结果。
- **评估命中口径收紧**：命中只看 Cause，不看 Facts。之前只看 Facts 会把错因判成通过，掩盖误判。

## 先决条件

- Go 1.23 或更新版本
- Mac 上跑真集群演示需要：Docker Desktop、kind、kubectl（见下文 kind 步骤）
- 本机测试不需要集群，`KUBE_MODE=fake` 就够了
- 真模型需要 API Key，只从环境变量读，不要写进代码、README 或任何提交的文件

## 安装

```bash
git clone <仓库地址>
cd kubecause
go mod download
cp .env.example .env   # 再填真实值，.env 已在 .gitignore 里，不会提交
```

Go 如果装在 `~/workspace/tools/go`，先把它的 bin 加进 PATH：

```bash
export PATH=$HOME/workspace/tools/go/bin:$PATH
go version  # 应显示 go1.23.x 或更新
```

## 测试

```bash
go vet ./...
go test ./...
go build ./...
```

也可以用 Makefile：

```bash
make test
make vet
make build
make eval       # stub 模式跑全部三个用例
make eval-real  # 真模型评估，无 Key 时明确跳过，不报错
```

## 环境变量一览

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `KUBE_MODE` | `fake` | `fake` 用 client-go fake clientset，预置 demo-api 数据；`kubeconfig` 用真 client-go 读 kubeconfig（kind、k3s、云厂商集群都走这个，`kind` 是别名） |
| `MODEL_MODE` | `stub` | `stub` 用桩模型，确定性走完工具调用；`real` 用真模型，Key 缺失时启动明确报错 |
| `MODEL_API_KEY` | 空 | 真模型 Key，`MODEL_MODE=real` 时必填，只从环境变量读 |
| `MODEL_BASE_URL` | `https://api.deepseek.com/v1` | 真模型地址；换 OpenAI 时改成 `https://api.openai.com/v1` |
| `MODEL_NAME` | `deepseek-chat` | 真模型名；换 OpenAI 时改成 `gpt-4o-mini` 等 |
| `DB_PATH` | `./kubecause.db` | SQLite 文件路径 |
| `ADDR` | `:8080` | 监听地址 |
| `KUBECONFIG` | 空 | kind 模式下指定读身份的 kubeconfig 路径，不填用默认 |
| `REPAIR_KUBECONFIG` | 空 | 写身份专用的 kubeconfig，不填时和读身份用同一个文件；生产上应指向写身份的文件 |
| `CONFIG_PATH` | `config.yaml` | 配置中心文件路径；不填时依次找 `config.yaml`、`../config.yaml`、`../../config.yaml`，都找不到用默认值跑 |
| `CHAT_MAX_STEPS` | 12 | 覆盖 `config.yaml` 的 `chat.maxSteps`，环境变量优先于文件 |
| `CHAT_MAX_HISTORY_MESSAGES` | 10 | 覆盖 `chat.maxHistoryMessages` |
| `RUN_MAX_STEPS` | 8 | 覆盖 `run.maxSteps` |
| `RUN_TIMEOUT_SECONDS` | 60 | 覆盖 `run.timeoutSeconds` |
| `MODEL_TIMEOUT_SECONDS` | 30 | 覆盖 `model.timeoutSeconds` |
| `MODEL_TEMPERATURE` | 0.2 | 覆盖 `model.temperature` |
| `REPAIR_MAX_MEMORY_MI` | 4096 | 覆盖 `repair.maxMemoryMi` |
| `REPAIR_MAX_RATIO` | 4 | 覆盖 `repair.maxRatio` |

## 启动

### fake + stub（默认，不需要集群，不需要 Key）

```bash
KUBE_MODE=fake MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
```

打开 http://localhost:8080 ，提交 `demo / Deployment / demo-api / demo-api 为什么一直重启`，就能看到工具步骤、时间线和诊断报告。

### kubeconfig + stub（Mac 上跑真集群，桩模型）

先在 Mac 上装 kind：

```bash
# 1. 装 Docker Desktop，启动后确认 docker ps 能跑
#    设置里内存调到 4G 以上，CPU 2 核以上
# 2. 装 kind 和 kubectl
brew install kind kubectl
kind version
kubectl version --client
# 3. 建集群
kind create cluster --name kubecause
kubectl get nodes
```

然后启动服务（`kind` 是 `kubeconfig` 的别名，两种写法都行）：

```bash
KUBE_MODE=kubeconfig MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
```

找不到 kubeconfig 时启动会明确报错，请确认已执行 `kind create cluster`，且 `~/.kube/config` 存在，或用 `KUBECONFIG` 指定。

云上跑 k3s 时也一样：k3s 的 kubeconfig 在 `/etc/rancher/k3s/k3s.yaml`，用 `KUBECONFIG` 指过去即可，不用换模式。

### kubeconfig + 真模型（Mac 上，DeepSeek 示例）

```bash
# Key 只从环境变量读，不要写进文件、不要提交
export MODEL_API_KEY="sk-..."
KUBE_MODE=kubeconfig MODEL_MODE=real MODEL_BASE_URL=https://api.deepseek.com/v1 MODEL_NAME=deepseek-chat DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
```

换 OpenAI 时只改两个变量：

```bash
export MODEL_API_KEY="sk-..."
KUBE_MODE=kubeconfig MODEL_MODE=real MODEL_BASE_URL=https://api.openai.com/v1 MODEL_NAME=gpt-4o-mini DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
```

## kind 演示脚本

```bash
# 先建好 kind 集群，再从仓库根目录跑
bash scripts/kind/setup-demo.sh
```

脚本会创建 namespace demo，应用 `scripts/kind/demo-api.yaml`，等到 Pod 出现 OOMKilled 或重启次数达标，并打印手动验证命令。演示用的 Deployment 是公开的 `python:3.12-slim` 镜像，容器不断分配内存，真实触发 OOM，不需要自己构建镜像，也不需要 `kind load docker-image`。

手动验证：

```bash
kubectl get pods -n demo
kubectl describe pod -n demo -l app=demo-api
kubectl logs -n demo -l app=demo-api --previous
```

清理演示：`kubectl delete -f scripts/kind/demo-api.yaml`

注意：本机（Muse 的 Linux VM）没有 Docker / kind，无法做 kind 端到端验证，脚本和上面的步骤就是给 Mac 上手动验证准备的。`KUBE_MODE=kubeconfig` 的 client-go 读 kubeconfig 逻辑已在 Slice 1 实现，Slice 2 没有改坏它，Slice 4 只是把 `kind` 改成了别名。

## 前端（Slice 5，React）

打开 http://localhost:8080 就是 React 页，不用另外起 Node 服务：

- **对话模式（主入口）**：左侧新建会话，中间问一句“当前状态怎么样”，再追问“demo 那个一直重启的是怎么回事”，工具执行过程会实时显示成时间线（正在执行的工具带转圈），总览、Pod 表、修复建议卡都渲染成卡片和表格；点“转成正式诊断”后右侧详情区出现任务、审批卡和修复验证。
- **单次诊断**：顶部切换到“单次诊断”，填 namespace / 类型 / 名字 / 问题，点提交，时间线每秒刷新，报告、审批卡、执行和验证结果在右侧详情区。
- **不用装 Node 直接跑**：交付 zip 里自带构建好的 `web/dist/`。要改前端时：`cd frontend && npm install && npm run build`（或 `make frontend`），再重启 Go 服务；开发模式用 `npm run dev`（Vite 在 :5173，/api 代理到 Go 服务 :8080）。

## 对话模式 curl 演示流程

服务启动后（fake + stub），在另一个终端跑：

```bash
# 1. 创建会话，返回会话 ID
curl -s -X POST http://localhost:8080/api/conversations \
  -H 'Content-Type: application/json' -d '{}'
# 记下返回的 id，例如 conv_xxxxxxxxxxxxxxxx，下面用 $CONV 代替

# 2. 第一轮：问当前状态，Agent 先看总览、再列有问题的 Pod
curl -s -X POST http://localhost:8080/api/conversations/$CONV/messages \
  -H 'Content-Type: application/json' \
  -d '{"content":"当前状态怎么样"}'
# 预期：reply 里有集群总览（节点数、命名空间数、有问题 Pod 数），
# structured 里有 overview 和 problemPods 的结构化结果

# 3. 第二轮：只说“demo 那个一直重启的是怎么回事”，不重新给 namespace，
# Agent 应接上文查 demo，返回结构化 Pod 列表和修复建议卡（128Mi -> 256Mi），
# 但不自动 Patch、不生成提案
curl -s -X POST http://localhost:8080/api/conversations/$CONV/messages \
  -H 'Content-Type: application/json' \
  -d '{"content":"demo 那个一直重启的是怎么回事"}'

# 4. 转成正式诊断：按会话历史解析出 demo/demo-api，创建任务，返回 runId
curl -s -X POST http://localhost:8080/api/conversations/$CONV/convert \
  -H 'Content-Type: application/json' -d '{}'
# 之后走老的 /api/runs 流程：等诊断完成、审批提案、Worker 执行并验证，
# 见下面“curl 演示流程”。
```

## 离线评估

```bash
# stub 模式：三个用例（OOM / 镜像 / 探针），应 3/3 通过
go run ./cmd/eval

# 真模型模式：有 Key 时用真模型跑同一批样本
MODEL_MODE=real MODEL_API_KEY="sk-..." go run ./cmd/eval

# 真模型模式但无 Key：明确跳过，不报错退出
MODEL_MODE=real go run ./cmd/eval
```

输出示例（stub）：每个用例一行 `case=... pass=true hit=true citation=1.00 steps=6 duration=...`，最后汇总命中率、平均步数、平均耗时、平均引用准确率。

## curl 演示流程

服务启动后（fake + stub），在另一个终端跑：

```bash
# 1. 创建任务，返回 202 和 runId
curl -s -X POST http://localhost:8080/api/runs \
  -H 'Content-Type: application/json' \
  -d '{"namespace":"demo","kind":"Deployment","name":"demo-api","question":"demo-api 为什么一直重启"}'

# 记下返回的 runId，例如 run_xxxxxxxxxxxxxxxx，下面用 $RUN_ID 代替

# 2. 轮询时间线（after 是已看到的最大序号）
curl -s "http://localhost:8080/api/runs/$RUN_ID/events?after=0"

# 3. 等 2 秒后读任务快照，能看到状态、诊断报告和待审批提案
curl -s "http://localhost:8080/api/runs/$RUN_ID"

# 4. 审批提案（批准后 Worker 自动执行并验证，拒绝后不执行）
#    先从上一步的 proposal.id 里拿到提案 ID，例如 chg_xxxxxxxxxxxxxxxx
curl -s -X POST "http://localhost:8080/api/runs/$RUN_ID/approvals/$CHG_ID/decision" \
  -H 'Content-Type: application/json' \
  -d '{"decision":"approved","approver":"varo"}'
```

预期结果：任务状态从 `queued` 到 `running` 再到 `completed`；时间线里有 `list_workload_pods`、`get_pod`、`get_events`、`get_pod_logs`、`get_deployment`、`search_runbooks` 六步；诊断报告的原因为“容器内存限制过低，触发 OOMKilled 导致反复重启”，证据里包含 OOMKilled；提案为 `128Mi -> 256Mi`，状态 `pending`。

# 5. 批准后等几秒再读任务快照，Worker 会自动执行并验证
curl -s "http://localhost:8080/api/runs/$RUN_ID"

预期结果：提案 `status` 为 `approved`，`executionStatus` 为 `executed`，`verificationStatus` 为 `passed`；`executionDetail` 里 `beforeValue` 是 `128Mi`、`afterValue` 是 `256Mi`、`dryRunOk` 为 true；`verificationDetail` 里新 Pod 已 Ready、重启次数未增长；时间线里多了两步 Kind 为 `repair` 的步骤，一步是“修复已写入”，一步是“验证通过”。拒绝时则相反：`executionStatus` 保持 `not_started`，Deployment 的 limit 不变。

## 目录说明

| 目录 | 职责 |
| --- | --- |
| `cmd/server/` | 入口：加载配置，组装模型、K8s 客户端、FTS5 索引和存储，启动 HTTP API 和 Worker |
| `cmd/eval/` | 离线评估：跑全部用例，输出命中率、引用准确性、步数和耗时汇总 |
| `internal/api/` | HTTP API：创建任务、查询快照、时间线轮询、审批决定 |
| `internal/run/` | Worker：领取任务、推进状态、重启恢复；工具适配器（FTS5 优先，关键词回退） |
| `internal/store/` | SQLite：迁移、任务、步骤、提案，步骤和进度同一事务提交 |
| `internal/chat/` | 对话模式：会话服务、Stub 确定性规则、真模型对话循环、修复建议卡合成、转正式诊断 |
| `internal/agent/` | Agent 循环：步数上限、时限、工具校验、证据引用校验、修复建议字段校验 |
| `internal/model/` | 模型适配器：stub 桩模型（三类场景确定性剧本），real 是 OpenAI 兼容协议的真模型 |
| `internal/kube/` | client-go 只读访问：fake 预置三类场景数据，kind 读 kubeconfig |
| `internal/knowledge/` | 排障手册检索：FTS5 索引（keywords 列解决中文短词查询）+ 关键词回退 |
| `internal/repair/` | 修复执行：白名单校验、条件 Patch、dry-run、写入后读回确认、rollout 验证（执行和验证分开存） |
| `scripts/kind/rbac.yaml` | RBAC 参考清单：只读身份和受写身份（写身份只能 patch demo-api） |
| `runbooks/` | 排障手册 Markdown |
| `testdata/cases/` | 评估用例：OOM、镜像拉取失败、探针失败 |
| `frontend/` | React 前端源码（Vite + TypeScript）：聊天、单次诊断、工具时间线、卡片和表格渲染 |
| `web/` | 前端构建产物（`web/dist/`，go:embed 打进二进制）+ 旧静态页 `web/index.html`（保留参考，不再托管） |
| `scripts/kind/` | kind 演示：demo-api.yaml 和 setup-demo.sh（OOM 案例）、demo-bad-image.yaml 和 setup-bad-image.sh（ImagePullBackOff 新案例，Slice 7） |

## Slice 2 已实现

- 真模型适配器：OpenAI 兼容 chat completions + tools，DeepSeek / OpenAI 只换环境变量；超时、重试、上下文截断；非法工具名交给 Agent 校验层拒绝
- FTS5 索引：启动时从 `runbooks/*.md` 建索引，按相关性排序，查不到回退到关键词检索
- 离线评估：三个用例在 stub 模式下 3/3 通过，输出命中率、引用准确性、步数、耗时；真模型无 Key 时跳过
- kind 演示脚本：公开镜像真实 OOM 的 Deployment + 一键 setup 脚本 + 手动验证步骤
- fake 客户端三类场景：OOM、镜像拉取失败、探针失败
- Slice 1 回归：fake + stub 全流程仍完成并生成提案

## Slice 3 已实现

- 修复执行：条件 Patch、dry-run、写入后读回确认、超时/中断后读回再下结论、过期和幂等处理
- rollout 验证：generation、副本数、新 Pod Ready、重启次数不增长，四项都达标且连续两轮才通过，超时标 timeout
- Worker 接入：批准后自动执行并验证，拒绝后无写入，重启后仍能领取
- 前端审批卡和修复展示（静态页），RBAC 参考清单
- 提案生成放宽到 high 或 medium，真模型 medium 也能出提案

## Slice 4 已实现

- 后端通用化：报告里的 `suggestedRepair`、通用白名单（任意目标、只改内存 limit、新值大于旧值、不超过 4 倍、不超过 4Gi）、通用提案生成（容器名、旧值、新值从集群现状补齐）、`KUBE_MODE=kubeconfig`（`kind` 是别名）
- 发现工具：`list_namespaces`、`list_deployments`、`list_problem_pods`、`get_cluster_overview`，全部只读，结构化返回
- 对话模式：会话 + 消息持久化、最近 10 条历史截断、指代消解、对话只读、修复建议卡、转正式诊断
- Runbook 扩到 8 篇，评估仍 3/3 通过；单次诊断、审批执行、rollout 验证全部回归通过

## Slice 8 已实现

- 对话步数 12 步（配置中心可改），步数用完返回半成品，不再直接报错
- 工具描述补全：只讲返回什么、覆盖多大范围，不写“不要调 xxx”
- 提示词“先给总览，等追问再深挖”，总览调几个工具由模型自己判断
- 轻量配置中心：`config.yaml` + 环境变量覆盖 + `GET/POST /api/config` 热加载 + 前端“设置”页，`MODEL_API_KEY` 绝不进配置中心

## Slice 8 未实现（仍未做）

- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控
- 配置中心不做多环境切换、配置版本管理，那是部署层面的事

## Slice 4 未实现（Slice 5 已实现）

- 完整 React + TypeScript 前端（Slice 5 已做，见上文）
- 系统命名空间读放开（Slice 5 已做：读能查、写仍拒绝）
- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控

## 架构文档

完整架构见 `ARCHITECTURE.md`（仓库外另存），架构图见 `docs/diagrams/architecture.png`。
