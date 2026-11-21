# 更新记录

按版本号倒序。每个版本对应一个 git tag，例如 `v0.9.0`。

## v0.12.0（2026-11-21）

v0.12.0 修的是用户在 2026-11-20 晚真机确认的双份时间线 bug：右侧活动面板里同一条工具调用显示两遍，耗时毫秒数完全相同（25/14/24/13 各两次，list_workload_pods 28ms 两次）。根因在代码里确认过：同一份调用存了两份——`recordTool` 每调一次写一条 tool 消息，`RunMessage` 一轮结束又把整轮的 tool_calls 抄进助手消息；前端按“消息 id + 调用 id”去重，tool 消息 id=101 和助手消息 id=107 的同一调用 id 变成两个 key，都留下了。修法按用户定的：用真关联，不用位置推导——messages 表加 `round_id`，同一轮的用户 / tool / 助手消息共享一个 `round_` 开头的 ID，助手消息不再抄 tool_calls。同一版本还加了用户要的模型请求调试：只打服务端日志，页面上什么都不加。

### 改了什么

- **工具调用只存一份，round_id 关联**（`internal/store/store.go`、`internal/chat/chat.go`、`internal/chat/real.go`、`internal/chat/stub.go`、`frontend/src/types.ts`、`frontend/src/App.tsx`）：messages 表加 `round_id` 列（老库自动迁移，老数据为空）；`RunMessage` 每轮生成一个 `round_` ID，`recordTool` 写进 tool 消息，助手消息存同一个；助手消息不再存 tool_calls（`Reply.ToolCalls` 仍在 POST 回复里返回，那是当次返回的临时数据，不是存储）。前端 `chatActivityItems` 只读 role=tool 的消息，按顺序列，去重 Map 删掉；每条助手消息下的“查了 N 个工具”改成数同 roundId 的 tool 消息（`types.ts` 的 `toolCallsForAssistant`）。老数据兼容：roundId 为空时回退到位置推导（上一条用户消息到本条助手消息之间的 tool 消息），再没有就读助手消息里存的老副本。`resolveNamespace` 还会扫历史的 ToolCalls，但只剩 tool 消息里那份——新轮次的命名空间解析靠用户文本、助手消息正文和 Structured 里的 namespace 字段，现有三轮记忆测试仍过。验证：新增 `TestRoundIDSharedAcrossRound`（两轮共享/区分 roundID、助手消息 tool_calls 为空）、`TestOldRoundIDEmptyIsReadable`（老数据行照常读）；前端逻辑用临时 node 脚本验过（新数据按 roundId 计数、活动列表恰好 3 条不重复、老数据两档回退都对）；curl fake+stub 两轮：round_1 是 1 用户 + 2 tool + 1 助手，round_2 是 1 用户 + 6 tool + 1 助手，助手消息 tool_calls 全空。
- **模型请求调试只打服务端日志**（`internal/chat/real.go`、`internal/chat/chat.go`、`cmd/server/main.go`）：`KUBE_DEBUG_MODEL=1` 启动时，`runReal` 每次调模型前打一条请求日志（实际发过去的完整数组，`[debug model] conv=... round=... step=N request=[...]`）、返回后打一条返回日志（调了哪个工具及参数，或最终文本 / 是否提交了诊断报告）。默认关闭；没有 POST 的 debug 参数、不往 API 回复里加调试字段、前端没有任何改动（没有开关、面板、console.log、`?debug=1`）。单条内容超 4000 字截断并注明原长，结构不丢；API Key 在请求头里，日志里不会出现；桩模型不调外部模型，不打这个日志。验证：真模型路径用 httptest 假服务端验过（`TestDebugLoggingWithMockModel`：第一次请求的数组是 system + 当前问题在最后一条，返回日志记下工具名，关闭时零日志，日志里无 Key）；curl 用本地 mock 模型服务端验过端到端：3 次请求（工具调用、最终文本、追问后的最终文本）的数组结构都对，step=2 的数组里含 assistant(tool_calls) + tool 往返，日志里无 Key；默认启动（不设环境变量）零调试日志；`KUBE_DEBUG_MODEL=1` + 桩模型时启动行出现、零 `[debug model]` 行（符合预期，桩模型不调模型）。
- **OOM 回归仍通**（curl fake+stub）：两轮对话出 128Mi→256Mi 修复建议卡 → 转换 `run_71898c5008438ad3` → 提案 `chg_18749b99b0085c54` → 批准 → executed + verification passed（新 Pod Ready）。

### 未实现（仍未做）

- 新问题还回上一轮答案（锚定）：已确认根因不只是历史里有上一轮诊断，还有 `runReal` 里不分问题类型、每轮都追问一次诊断报告的逻辑（`askedForStructured`）和 `formatReportReply` 替换掉模型原文的做法。用户明确说先搁置这个 bug，提示词那条也不加，等 v0.12.0 验证完再看。
- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控
- 白名单做成可配置（用户说复杂先不做）
- op 只支持 replace：add/remove 以后需要时再加，现在不做，避免删字段
- command/args、hostNetwork、privileged 这些字段先禁掉，以后用户明确要求时再放开

## v0.11.0（2026-11-20）

v0.11.0 修的是用户在 2026-11-20 晚反馈的 5 个问题：**审批后不知道在干嘛**（批完页面不反馈状态，要再问一句才刷出来）、**点转换按钮后不自动滚到审批卡**、**聊多了上下文混乱**（答案停留在上次问的问题上）、**右侧活动面板看不到最新工具调用**、**创建追问里出现内部字段名**（模型把 `suggestedCreation` 直接写给用户看）。根因都在代码里确认过，不是感觉问题：审批轮询的启动门控、历史按全量行数截断、当前问题在模型输入里出现两次，都是确定性的 bug。

### 改了什么

- **批完无条件轮询，审批卡上给进度线**（`frontend/src/App.tsx`、`frontend/src/components.tsx`）：`decide()` 批完只读一次任务、再按 `runStillActive` 决定要不要轮询——批完那一刻任务已是 completed、提案执行状态还是 `not_started`（Worker 每 500ms 才来领），判断是 false，轮询根本起不来；`RepairStatusCard` 在执行和验证都是 `not_started` 时直接返回 null，审批卡只显示“已决定”，没有任何进度。现在批完不管快照状态如何都启动轮询，一直轮到提案到终态（executed/failed/expired，验证 passed/failed/timeout）再停，上限 400 次（约 6 分钟）兜底；`loadRunById` 同样的门控一起修。`RepairStatusCard` 在已批准 + `not_started` 时渲染“已批准，等待 Worker 执行…”和一条进度线“✓ 已批准 → ◌ 执行 → · 验证 → · 完成”，不再返回 null；审批还没决定时仍不显示。验证：fake+stub 下 OOM 转换 → 提案 `chg_97aae4e2afb29e3f` → 批准，批完立即读正是 completed + `not_started`（旧前端轮询起不来的那一刻），继续轮询看到 executed + verifying → executed + passed。
- **点转换后强制滚到审批卡**（`frontend/src/App.tsx`）：滚动 effect 只在 `nearBottomRef` 为真时才跟滚，用户点转换按钮时建议卡经常不在最底下（往上翻找到卡点的，或拿卡后又追问了几句），审批卡插在锚点消息后面、在可视区外。现在转换是用户主动操作，点完用 `scrollIntoView` 把审批卡滚进视野，不走“来了条新消息才跟滚”的逻辑；创建建议卡的转换按钮同样处理。VM 里没有真浏览器，这条是代码级验证 + Mac 手动验证步骤，见下面。
- **历史只算用户/助手消息**（`internal/chat/chat.go`、`internal/chat/real.go`、`internal/config/config.go`、`config.yaml`、`README.md`、前端设置页文案）：`lastMessages` 以前按数据库全量行数取最近 N=10 条，所有角色都算；每次工具调用单独存一条 tool 消息，一轮对话 = 1 条用户 + 6~8 条 tool + 1 条助手，约 10 条，所以“最近 10 条”实际只够覆盖一轮，问两三轮后最早那轮的目标就被截掉了。现在只算 user 和 assistant，tool 消息不占名额；本轮调工具的结果由真模型循环实时追加进模型输入，上一轮的工具细节丢掉，但助手消息里的结论和事实摘要还在，能接上文。`resolveNamespace` 用的就是这份历史，同样按这个口径走。`config.yaml` 的 `maxHistoryMessages: 10` 含义改成“最近 10 条用户/助手消息”，配置说明、README 表格和设置页文案一起改。注意：没有做假的对话摘要，历史摘要是以后 roadmap 的单独一项。验证：新增测试构造 3 轮含 tool 消息的历史（24 行），确认模型输入里仍有第一轮的用户问题；6 轮时截到 10 条用户/助手消息，第一轮被截掉、第二轮还在。curl fake+stub 三轮：第一轮问 demo-api，第二轮问总览，第三轮不给 namespace 只说“那个一直重启的现在重启了几次”，仍解析到 demo、深挖 demo-api（OOMKilled、重启 5 次）。
- **当前问题在模型输入里只出现一次**（`internal/chat/real.go`）：`RunMessage` 先把用户消息落盘，`history` 里已经有这句了，`runReal` 组装 `msgs` 时又在最后追加了一遍同样的 `text`，当前问题出现两次，模型会过度看重最新一句、跟着重复。现在不再追加，`history` 的最后一条就是当前问题。新增测试用 mock 真模型抓请求体确认：两次请求里当前问题都恰好出现一次。
- **右侧活动面板自动滚动**（`frontend/src/App.tsx`、`frontend/src/index.css`）：`ActivityPanel` 没有任何滚动逻辑，列表是旧的在上、新的在下，越聊越长，最新在可视区外面。顺序留在下面（新的在下，和聊天、时间线阅读顺序一致），加自动滚动，复用聊天那套模式——已经在最底下就跟着滚到底，往上翻了显示“↓ 有新内容”按钮，不把人拽下来；正在执行的卡片在最下面，高亮加自动滚才能看到。VM 里没有真浏览器，代码级验证 + Mac 手动验证。
- **提示词堵内部字段名外泄**（`internal/chat/real.go`）：截图里的 `demo-http`、`hashicorp/http-echo` 在代码里一个都没有，是真模型按提示词第 10 条现编的追问，不是写死的，不用改；要改的是模型把 `suggestedCreation` 直接写给用户看。`chatSystemPrompt` 加第 11 条：给用户看的回答里不要提任何内部字段名、工具名、结构化字段名（例如 suggestedRepair、suggestedCreation、submit_diagnosis、get_pod），只说人话；`runReal` 里那段追问结构化建议的文本也加了同样一句。

### 未实现（仍未做）

- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控
- 白名单做成可配置（用户说复杂先不做）
- op 只支持 replace：add/remove 以后需要时再加，现在不做，避免删字段
- command/args、hostNetwork、privileged 这些字段先禁掉，以后用户明确要求时再放开

## v0.10.0（2026-11-20）

v0.10.0 修的是用户在 2026-11-20 反馈的两个问题：**转正式诊断按钮没带目标**（demo-bad-image 换镜像的建议卡，点了“创建可审批的修复任务”，建出来的却是 demo/Deployment/demo-api）和**正式诊断重跑失败**（建出来的 run 自己重新诊断，8 步用完、置信度 low、事实重复、没有提案，修复任务从来没成功过）；另外按用户要求**加了一个通用创建能力**（对话里说“在 demo 空间下创建一个 Pod/Deployment”，现在能走建议卡 → 审批 → 执行 → 验证），以后想改成 manager agent 时复用，不为每个操作单独写一套流程。

### 改了什么

- **转换带目标，不再猜**（`frontend/src/App.tsx`、`frontend/src/components.tsx`、`frontend/src/api.ts`、`frontend/src/types.ts`、`internal/api/api.go`、`internal/chat/chat.go`）：建议卡上的转换按钮把这张卡的目标（namespace/kind/name/container）和 Patches 一起传过去，`POST /api/conversations/{id}/convert` 接受可选的 namespace/kind/name/container/patches/creation；后端 `ConvertToRun` 优先用传来的目标，不再用 `resolveTargetFromHistory` 猜——以前只传会话 id，猜到的是历史里最早的 demo-api。什么都没传时才回退到历史猜测（老客户端兼容）。创建卡的转换按钮传 creation 规格，按钮文案是“创建可审批的创建任务”。
- **Worker 用带过来的建议直接生成提案，不重新诊断**（`internal/store/store.go`、`internal/run/worker.go`）：`runs` 表加 `suggested_json` / `creation_json` 两列（老库启动时自动补列），`ConvertToRun` 把建议卡的 Patches / 创建规格存在 Run 上；Worker 看到 Run 带建议就跳过 Agent 循环，直接按建议生成一份 medium 置信度的报告（事实是“用户在对话里已确认修复/创建建议：……”），再走原来的 `maybeCreateProposal` 校验、生成提案。提案生成时仍要读集群现状核对（容器下标按容器名重算、改前值按现状读、过 `ValidatePatch` / `ValidatePatchWithCurrent`），校验不过不生成提案。验证：fake+stub 下两轮对话后点 demo-bad-image 的卡（传 nginx:9.9.9-nonexistent → nginx:1.24 的 image Patch），建出 demo/demo-bad-image 的 run，不再是 demo-api；OOM 回归仍给 128Mi→256Mi 内存卡，审批后执行验证通过。
- **报告事实去重**（`internal/agent/agent.go`）：`Report.Normalize()` 里事实、建议、待确认、证据 ID 各去重一次，顺序不变。以前正式诊断里同一条事实出现了两遍（“Deployment demo-api, container demo-api memory limit 256Mi”重复），原因是多步工具摘要拼出来的事实本身就有重复，归一化时不去重就原样落盘、原样展示。去重只在归一化这一层做，不改工具摘要本身，工具时间线仍能看到每一步的原始记录。
- **通用创建能力**（新增 `internal/repair/create.go`，改 `internal/agent/agent.go`、`internal/model/model.go`、`internal/chat/chat.go`、`internal/chat/stub.go`、`internal/chat/real.go`、`internal/run/worker.go`、`internal/repair/repair.go`、`internal/kube/client.go`、`internal/store/store.go`、前端）：建议卡新增一种创建类型（`SuggestedCreation`，即 `store.CreationSpec`：namespace、name、image、replicas、container、内存/CPU 的 requests/limits、reason），和修复建议卡平级。模型流程：用户说“在 demo 空间下创建一个 Pod/Deployment”，模型先问缺什么（命名空间、名字、镜像，副本数默认 1 不用问），问全了直接给一张创建建议卡，不用先做故障诊断；用户说创建 Pod 时也按 Deployment 给卡，并在回复里说明裸 Pod 不会自动重建、建议用 Deployment；用户分几轮给的信息（名字、镜像）靠会话历史拼在一起解析，不用重说一遍。`submit_diagnosis` 加 `suggestedCreation` 字段，对话和单次诊断的提示词各加一句创建规则，不写“不要调 xxx”这种禁止句。提案分两种：`change_requests` 表加 `kind`（`patch` / `create`，老提案读出来是 `patch`）和 `creation_json` 两列，`ChangeRequest` 加 `Kind` / `Creation` 字段；创建提案走同一套审批 / 执行 / 验证流程，只是执行动作从 Patch 换成 `RepairClient.CreateDeployment`（Real 用 client-go Create，Fake 在内存里模拟 + 复用 `simulateRollout`），验证共用同一套 rollout 验证（observedGeneration、新 Pod Ready、重启次数不增长）。创建校验 `ValidateCreation` 复用 Patch 的思路：只允许创建 Deployment、系统命名空间拒绝、镜像名合法、副本数 1–10、内存/CPU 在上限内（复用 `repair.maxMemoryMi` / `repair.maxRatio` 的配置）；执行前再查一次目标不存在才能创建，已存在且配置一致视为已创建（幂等），已存在且配置不一致标过期。前端：创建建议卡和 Patch 建议卡长得一样，只是标题和字段不同，按钮文案是“创建可审批的创建任务”；审批卡按 kind 分两种展示，创建提案显示镜像、副本数、资源，不按 Patch 列表展示。
- **回归**：`go vet ./...` 通过，`go test ./...` 全部通过（含新增的转换带目标、Worker 用自带 Patch/创建建议生成提案、创建校验、Fake 创建+验证全流程、Stub 创建多轮、报告去重、提案 kind/创建规格持久化测试），`go run ./cmd/eval` 仍 3/3 通过；fake+stub 下 curl 验证三段全通（OOM 回归转换 → 提案 → 批准 → 执行验证通过；demo-bad-image 目标转换 → 建出 demo-bad-image 的任务；创建 web-demo → 创建提案 → 批准 → 执行验证通过，总览 Deployment 数 1 → 2）。

### 未实现（仍未做）

- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控
- 白名单做成可配置（用户说复杂先不做）
- op 只支持 replace：add/remove 以后需要时再加，现在不做，避免删字段
- command/args、hostNetwork、privileged 这些字段先禁掉，以后用户明确要求时再放开

## v0.9.0（2026-11-20）

v0.9.0 修的是用户在 2026-11-20 反馈的三个问题：**整页滚动**（输入框还是被顶走，时间线看不到）、**只有 OOM 能出修复建议**（ImagePullBackOff 没有卡）、**每个故障都要单独写代码吗**（执行器只认内存）。核心是两件事：**布局锁死一屏**（`.app` 从 `min-height:100vh` 改成 `height:100vh; overflow:hidden`，三栏各自内部滚动）和**执行器改成通用 Patch**（建议卡、校验、执行、提案、审批卡全部按 op/path/value 走，不再按故障类型分分支，OOM、ImagePullBackOff、主动换镜像版本都走同一套流程）。

v0.1.0–v0.8.0 已有的东西没有重写：SQLite 表（只给 change_requests 加了一列 patches_json，老库启动时自动补列）、HTTP 接口、Worker 状态机、Agent 循环的校验和恢复、fake 客户端、桩模型、真模型适配器、FTS5 检索、条件 Patch 与 rollout 验证、对话会话、发现工具、React 前端，全部保留并做了回归。

### 改了什么

- **修布局：三栏各自独立滚动**（`frontend/src/index.css`）：根因是 `.app` 是 `min-height:100vh` 不是 `height:100vh`——内容一长整个页面就比屏幕高，浏览器出现整页滚动条，`.main`、`.sidebar`、`.activity` 的内部滚动根本触发不了，输入框也被顶走。修法：`html, body` 加 `height:100%; overflow:hidden`，`.app` 改成 `height:100vh; overflow:hidden`，高度锁死在一屏；`.layout` 保持 `flex:1; min-height:0`，三列高度都被锁住；左边 `.sidebar`、中间 `.main`、右边 `.activity` 各自 `overflow-y:auto`，自己滚自己的，互不影响；中间聊天区内部再分上面 `.chat-scroll` 自己滚、下面 `.composer`（`flex-shrink:0`）固定在底部；单次诊断的 `.run-scroll` 也一样处理。验证：20 条以上长对话里，滚中间聊天时右侧活动面板不动、一直能看到工具卡片，滚右侧时中间不动，输入框一直固定在底部。
- **执行器改通用 Patch**（`internal/agent/agent.go`、`internal/repair/repair.go`、`internal/store/store.go`、`internal/kube/client.go`、`internal/run/worker.go`）：`SuggestedRepair` 不再是 namespace/deployment/container/field/oldValue/newValue 这种只认内存的形状，改成通用 Patch 列表 `Patches []PatchOperation`，每条是 `{op, path, value}`，op 只支持 `replace`（不做 add/remove，避免删字段），path 例如 `/spec/template/spec/containers/0/image`、`/spec/template/spec/containers/0/resources/limits/memory`、`/spec/replicas`，value 例如 `"nginx:1.25"`、`"256Mi"`、`3`。校验改成 `ValidatePatch`（只按字段分，不按故障分）+ `ValidatePatchWithCurrent`（现状相关：容器下标越界、内存倍数/上限）：只允许改 Deployment，白名单 path 只有副本数（1-10）、容器镜像（合法镜像名）、容器内存/CPU 的 limit/request；黑名单明确拒绝 hostNetwork、hostPID、privileged、metadata/name、namespace、command、args；系统命名空间拒绝；内存新值大于旧值、不超过旧值 4 倍、不超过 4Gi。执行改成 `kube.RepairClient.PatchDeployment`，直接用 client-go 的 Patch API（JSONPatchType）把模型给的 Patches 列表原样发出去，执行前先 dry-run，Patch 前重读一次核对 resourceVersion 没变（JSONPatch 体里没有放 resourceVersion 的位置，这一步和原来把 resourceVersion 写进 strategic merge patch 的 metadata 是同一层保证），执行后逐条读回确认（镜像读 image、内存读 limits.memory、副本数读 replicas，不一致就标记执行失败）。rollout 验证保持原来的逻辑（observedGeneration、updated/available replicas、新 Pod Ready、重启次数不增长），没改。Worker 的 `maybeCreateProposal` 改成通用：按建议卡的 Patches 生成提案，容器下标按容器名重算，改前值逐条按现状读，第一个 Patch 的改前/改后值存进 OldValue/NewValue 做展示；`change_requests` 表加 `patches_json` 列，`ChangeRequest` 加 `Patches` 字段。前端 `RepairSuggestionCard` 和审批卡改成显示 Patch 列表（字段中文名 + path + 新值，旧值读不到只显示新值，不编旧值）。
- **向后兼容**（不删旧逻辑）：`SuggestedRepair` 里旧的 Field/OldValue/NewValue 字段保留，`Normalize()` / `ToPatches()` 会把旧的 memory-only 卡转成一条内存 limit 的 Patch；`repair.ValidatePatchSpec` 保留，内部转成通用 Patch 再校验；`store` 读老提案（patches_json 为空）时从 oldValue/newValue 反推一条内存 Patch；`kube.PatchDeploymentMemoryLimit` 保留，老测试照常用。`go run ./cmd/eval` 仍 3/3 通过。
- **多轮追问选镜像**（`internal/chat/real.go`、`internal/chat/stub.go`）：模型问“你想换成哪个镜像？是 nginx:1.25 还是 nginx:latest？”之后，用户回“换成 nginx:1.25”，下一轮模型把用户的回答转成结构化 Patch，而不是再问一遍。多轮记忆靠最近 10 条历史（已经有了）：提示词加了一句“用户上一轮已经明确说了新值时，直接用那个值生成结构化 Patch，不要再问一遍”。Stub 路径也支持：消息里有镜像名 + 换/改/升级，或者上一轮刚问过选哪个镜像、这轮只回了一个镜像名，就直接查当前镜像、给 Patch 卡。mock 模型服务两轮对话测试已覆盖。
- **主动变更走同一套 Patch**（`internal/chat/real.go`、`internal/chat/stub.go`）：用户说“帮我把 demo-api 的镜像换成 nginx:1.26”，Pod 是健康的，不用诊断，直接查一下当前镜像是什么，确认一下，然后给一张 Patch 建议卡，点了“创建可审批的修复任务”，审批后执行，再验证 rollout。提示词加了一句“用户明确说了要改成什么值时，不用先做故障诊断：先查当前值确认一下，再直接给出 Patch 建议”。mock 模型 + Stub 两路测试已覆盖（主动变更只调 get_deployment，不调 list_problem_pods、get_pod、get_events 这些诊断工具）。
- **提示词通用化**（`internal/chat/real.go`、`internal/model/model.go`）：对话提示词第 6、7 条和单次诊断提示词第 5、7 条不再只写内存，改成“用 patches 列表描述要改的字段”；`submit_diagnosis` 的 suggestedRepair 工具定义加了 `patches` 数组字段，旧的 field/oldValue/newValue 保留做兼容。提示词和工具描述里不写“不要调 xxx”这种禁止句，只描述每个工具返回什么、Patch 白名单里有什么，让模型自己判断。
- **Stub 的镜像建议**（`internal/model/model.go`、`internal/chat/chat.go`，测试路径专用）：Stub 的 imagepull 场景现在也给一张镜像 Patch 卡（demo-api:bad-tag → demo-api:latest），和 OOM 的内存卡走同一套通用 Patch；`synthesizeRepair` 也支持镜像卡。注意：这两处都是桩模型测试路径的确定性规则，真模型路径不走这里；真集群上的坏镜像 tag 各不相同，新镜像只能由模型按证据和用户回答决定，规则合成猜不出一个正确的 tag。另外修了一个相关 bug：`synthesizeRepair` 以前把 runbook 检索片段也当成一手证据，查 ImagePullBackOff 时 OOM 手册里的 "out of memory" 会把镜像案误判成内存案，现在检索片段只看摘要、不参与信号判断。
- **回归**：`go vet ./...` 通过，`go test ./...` 全部通过（含新增的通用 Patch 白名单、通用 Patch 执行（镜像/内存/副本数/多 Patch）、多轮追问、主动变更、Stub 镜像卡、提案 Patch 持久化、旧卡向后兼容测试），`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、转正式诊断后审批执行验证全部回归通过。

### 未实现（仍未做）

- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控
- op 只支持 replace：add/remove 以后需要时再加，现在不做，避免删字段
- command/args、hostNetwork、privileged 这些字段先禁掉，以后用户明确要求时再放开

## v0.8.0（2026-11-20）

v0.8.0 修的是用户在 Mac 真机上反馈的“当前状态怎么样”8 步用完报错的问题（2026-11-20）：对话里问一句总览，页面中间一片空白，下面只有一句粉色报错“对话已达最大工具步数（8 步），请换个更具体的问题再问”。核心是三件事：**步数是预算不是决策**（MaxChatSteps 从 8 调到 12，步数用完返回已查到的半成品，不再直接报错）、**工具描述只描述、不禁止**（讲清每个工具返回什么、覆盖多大范围，删掉“不要调 xxx”这种替模型做决策的禁止句）、**提示词改成先给总览、等追问再深挖**（一轮里不要既看全貌又深挖）。另外加了一个轻量配置中心：`config.yaml` + 环境变量覆盖 + `GET/POST /api/config` 热加载，前端“设置”页能改，不用重启。

v0.1.0–v0.7.0 已有的东西没有重写：SQLite 表、HTTP 接口、Worker 状态机、Agent 循环的校验和恢复、fake 客户端、桩模型、真模型适配器、FTS5 检索、条件 Patch 与 rollout 验证、对话会话、发现工具、React 前端，全部保留并做了回归。

### 改了什么

- **MaxChatSteps 从 8 改成 12，步数用完返回半成品，不再直接报错**（`internal/chat/chat.go`、`internal/chat/real.go`）：根因不是模型不会决策，是预算太小 + 预算用完时后端直接 `return error`，RunMessage 把错误丢回前端，用户看到的是空白加一句“请换个更具体的问题再问”——等于把预算用完的锅推给用户，问题本身（“当前状态怎么样”）一点错没有。修法两层：预算调到 12（对话专用，单次诊断的 8 步不动）；`runReal` 循环跑完 12 步还没收敛时，把已经查到的工具结果（`outputs` 的摘要 + `structured` 的规范 key）整理成半成品回复返回，文字写“已经查到了这些，但步数用完了，还没来得及总结：”然后列出已查到的总览和问题 Pod，再问“你想深入查哪个命名空间或 Pod？”。RunMessage 把半成品正常落盘成一条助手消息，前端按 `structured` 里的 `overview` / `problemPods` 正常渲染卡片和表格，刷新后也还在。MaxChatSteps 是预算不是决策：调大它不干预模型怎么选工具，只是允许一轮里查得更多。
- **工具描述补全，只描述、不禁止**（`internal/model/model.go` 的 `toolDefs`）：每个工具的 description 现在讲清返回什么、覆盖多大范围，例如 `get_cluster_overview` 一次返回全集群汇总（节点数、命名空间数、Deployment 数、Pod 数、有问题 Pod 数），`list_problem_pods` 不带 namespace 时一次返回全部命名空间的有问题 Pod、带时只返回那个命名空间的，`list_namespaces` 只返回名字列表、不返回 Pod 状态，`list_deployments` 返回某个命名空间的 Deployment 列表、不返回 Pod 详情。描述里不写“不要调 xxx”“不再需要调 xxx”这种禁止句——该调哪个由模型按返回内容自己判断，写死禁止规则是替模型做决策。新增测试 `TestToolDescriptionsDescribeOnly`，描述里出现禁止句就失败。
- **提示词改成“先给总览，等追问再深挖”**（`internal/chat/real.go` 的 `chatSystemPrompt`）：原来“先用发现工具看全貌、找问题，再用定向工具深挖”会鼓励模型一轮里既看全貌又深挖，浪费步数。现在改成：总览问题先查全集群状态、给出摘要就停下来，等用户追问某个命名空间或 Pod 时再深挖，不要在一轮里把所有命名空间、所有 Pod 都查完。没有写死“总览就是 2 个工具”——调几个工具由模型自己判断。指代消解、只读、诚实、修复建议必须结构化返回这几条原样保留。
- **配置中心（轻量）**（新增 `internal/config/config.go`、`config.yaml`，`internal/api/api.go`、`cmd/server/main.go`）：配置文件 `config.yaml` 放仓库根目录，字段有 `chat.maxSteps`（12）、`chat.maxHistoryMessages`（10）、`run.maxSteps`（8）、`run.timeoutSeconds`（60）、`model.timeoutSeconds`（30）、`model.temperature`（0.2）、`repair.maxMemoryMi`（4096）、`repair.maxRatio`（4）。启动时先读文件，没有文件用默认值；环境变量可以覆盖（`CHAT_MAX_STEPS`、`CHAT_MAX_HISTORY_MESSAGES`、`RUN_MAX_STEPS`、`RUN_TIMEOUT_SECONDS`、`MODEL_TIMEOUT_SECONDS`、`MODEL_TEMPERATURE`、`REPAIR_MAX_MEMORY_MI`、`REPAIR_MAX_RATIO`），环境变量优先于文件。`GET /api/config` 读当前配置，`POST /api/config` 改配置：校验范围（非法值例如 `maxSteps=0`、`temperature=2` 返回 400，不保存、不生效），保存到 `config.yaml`，热加载不用重启。`MODEL_API_KEY` 绝不进配置中心：配置结构里没有这个字段，GET 不返回，前端不显示、不让改，Key 只从环境变量读。`internal/chat` 的步数/历史数、`internal/store` 的任务创建默认值、`internal/repair` 的上限（`MaxMemoryMi` / `MaxRatio` 从常量改成变量）、真模型的超时/温度，启动和热更新时都从配置中心读。
- **前端“设置”页**（`frontend/src/App.tsx`、`frontend/src/api.ts`、`frontend/src/types.ts`、`frontend/src/index.css`）：header 旁边加了一个“设置”按钮，点开弹窗，8 个参数各一个输入框、带一句中文说明（例如“对话最大工具步数：模型一轮最多调几次工具，调大可以查得更全，但更慢更贵”），点“保存”调 `POST /api/config`，保存后显示“已保存，立即生效，不用重启”。半成品回复不需要前端特殊处理：它就是一条普通的助手消息，`StructuredView` 按 `overview` / `problemPods` 正常渲染卡片和表格；旧的“请换个更具体的问题再问”那句报错后端已经不再返回，前端也不再显示它。
- **回归**：`go vet ./...` 通过，`go test ./...` 全部通过（含新增的配置加载/校验/热更新、步数用完返回半成品、配置中心 GET/POST、工具描述无禁止句、修复上限跟配置五个测试），`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、转正式诊断后审批执行验证全部回归通过。

v0.1.0–v0.7.0 已有的东西没有重写：SQLite 表、HTTP 接口、Worker 状态机、Agent 循环的校验和恢复、fake 客户端、桩模型、真模型适配器、FTS5 检索、条件 Patch 与 rollout 验证、对话会话、发现工具、React 前端，全部保留并做了回归。

### 已实现

- 对话步数 12 步（配置中心可改），步数用完返回半成品，不再直接报错
- 工具描述补全：只讲返回什么、覆盖多大范围，不写“不要调 xxx”
- 提示词“先给总览，等追问再深挖”，总览调几个工具由模型自己判断
- 轻量配置中心：`config.yaml` + 环境变量覆盖 + `GET/POST /api/config` 热加载 + 前端“设置”页，`MODEL_API_KEY` 绝不进配置中心

### 未实现（仍未做）

- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控
- 配置中心不做多环境切换、配置版本管理，那是部署层面的事

## v0.7.0（2026-11-20）

### 改了什么

v0.7.0 修的是用户在 Mac 真机（kind + 真模型）上反馈的 6 个问题，外加一个全新的故障案例 ImagePullBackOff，供用户测试 Agent 的通用发现能力。核心是三件事：**白屏不再发生**（后端报告空数组归一化 + 前端判空 + 错误边界）、**聊天不再是纯文本**（后端真模型路径存规范 key + 前端轻量 Markdown 渲染）、**正式诊断的按钮和位置说清楚**（只在有建议卡时显示，按时间顺序插进消息流）。

- **修白屏（最优先）**（`internal/agent/agent.go`、`internal/model/model.go`、`internal/chat/real.go`、`frontend/src/components.tsx`、`frontend/src/types.ts`）：根因是真模型返回的 JSON 缺字段时，Go 解出来是 nil，再序列化成 JSON 就是 null，前端读 `report.facts.length` 直接抛错，整个 React 应用白屏。修法三层：后端 `Report.Normalize()` 把 facts、evidenceIds、recommendations、openQuestions 缺失时补成空数组，`ValidateReport` 和 `parseDiagnosisArgs` 都会调它，序列化出来是 `[]` 而不是 null；前端 `normalizeReport` 再兜一层，历史库里旧的 null 报告也能渲染，`ReportView`、`OverviewCards`、`PodTable`、`DeploymentTable`、`buildBlocks` 里所有 `.length`、`.map` 的地方都先判空；再加一个 React `ErrorBoundary` 包住聊天消息区和正式诊断卡片，某个卡片渲染出错时只显示“这个卡片渲染失败了，请刷新重试”，不让整个页面白屏。新增测试 `TestReportNormalizeEmptyArrays` 和 `TestValidateReportNormalizes`。
- **正式诊断按钮说清楚 + 只在有建议时显示 + 快照按时间顺序**（`frontend/src/App.tsx`、`frontend/src/components.tsx`）：按钮文案从“转成正式诊断”改成“创建可审批的修复任务”，下面加一句小字解释：“生成一份带提案的诊断任务，人工审批后才会执行，不会直接改集群。”输入框旁边的按钮删掉了——没有建议卡时点它没有意义，入口只在修复建议卡上。快照卡片不再固定在聊天底部：转换时记下最后一条消息的 id 作为锚点，审批卡插在那条消息后面按时间顺序出现，之后再发的新问题会出现在它下面。**聊天和正式诊断的区别**：聊天是探索，只读不改，没有审批，没有执行记录；正式诊断是一个任务，有 runId、有状态、有步骤、有报告、有提案、有审批、有执行和验证，全都存在数据库里，能审计，能重启后恢复。按钮存在是因为聊天不能直接动手改集群，必须先生成一个正式任务走人工审批——这是安全边界，不是多余的一步。
- **runbooks 不是特例**（不用改代码，界面和文档写清）：`runbooks/*.md` 是 RAG 知识库，8 篇 Markdown，SQLite FTS5 索引，模型通过 `search_runbooks` 工具自己搜，不是代码里 `if OOM 就返回某篇`。评估的 3 个用例是测试夹具，不是产品逻辑。界面上现在能看到引用了哪篇手册：`search_runbooks` 返回的片段带标题、来源文件和关键句，前端按 `runbooks` 结果块渲染成“参考手册”，不再只显示 ev-1、ev-2 这种编号。加一篇新的 `.md` 文件重启就进索引，模型就能搜到。
- **后续追问不再纯文本 + 聊天记录 Markdown 渲染**（`internal/chat/real.go`、`frontend/src/components.tsx`、`frontend/src/types.ts`）：根因是真模型路径只把工具结果存进 `structured[工具名]`（例如 `structured["get_pod"]`），前端 `buildBlocks` 只认规范 key，工具名当 key 的结果会被过滤掉，聊天里就只剩文字、没有卡片，刷新后也一样。现在每个工具的结果同时写一份规范 key：`get_cluster_overview` → `overview`、`list_problem_pods` → `problemPods`、`get_pod` → `pod`、`get_deployment` → `deployment`、`list_deployments` → `deployments`、`get_events` → `events`、`search_runbooks` → `runbooks`，工具名那份留着做追溯，两份都落盘，刷新后卡片还在。前端加了一个手写的轻量 Markdown 渲染（不引入重型库，100 行以内）：助手消息支持 `**加粗**`、`` `代码` ``、`- 无序列表`、`1. 有序列表`和分段，渲染失败时 fallback 回纯文本。新增测试 `TestRealCanonicalKeys`（mock 模型服务按顺序返回两个工具调用再给纯文本，验证 Structured 里有 overview 和 problemPods，刷新后还在）。
- **输入框固定底部**（`frontend/src/App.tsx`、`frontend/src/index.css`）：以前输入框在能滚动的 `.main` 里面，消息一长就被顶出去了。现在 `.main` 本身不再滚动，里面分两块：上面 `.chat-scroll`（聊天）/ `.run-scroll`（单次诊断）自己滚动，下面 `.composer` 固定在底部不跟着滚。只改聊天模式，单次诊断的表单不动。
- **新案例：ImagePullBackOff**（`scripts/kind/demo-bad-image.yaml`、`scripts/kind/setup-bad-image.sh`，见下文“新案例”一节）：namespace demo，Deployment demo-bad-image，镜像 `nginx:9.9.9-nonexistent`，Pod 卡在 ImagePullBackOff。代码里没有任何针对这个案例的特殊处理：Go 代码里没有 `demo-bad-image` 这个字符串，没有为它加 runbook（已有的 `runbooks/imagepull.md` 已够用），fake 客户端里也没有预置它。这个案例只在真集群上测，用来验证 Agent 能不能靠通用工具自己发现问题。
- **回归**：`go test ./...` 全部通过（含新增的报告归一化、真模型规范 key 两个测试），`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、转正式诊断后审批执行验证全部回归通过。

### 新案例：ImagePullBackOff（demo-bad-image）

这是 v0.7.0 新增的第二个故障案例，用来测试 Agent 的通用发现能力——它能不能发现一个代码里完全没特殊处理过的问题。

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

## v0.6.0（2026-11-20）

### 改了什么

- **工具时间线移到右侧活动面板**（`frontend/src/App.tsx`、`frontend/src/components.tsx`）：布局改成左侧会话列表、中间聊天、右侧“Agent 活动”面板。右侧不再放诊断报告、审批卡、修复验证，只放工具执行过程：每一步一张卡片，显示工具名、参数摘要、状态（进行中带转圈高亮 / 成功打勾 / 失败打叉标红）、结果摘要、耗时，卡片可点开展开看详情。聊天里不再贴完整时间线，只留一行摘要，例如“查了 6 个工具 · 总耗时 12ms”，点一下展开看完整时间线。单次诊断也一样：任务区只留一行摘要，完整时间线在右侧面板。
- **工具调用带耗时**（`internal/chat/`）：对话的 `ToolCallRecord` 新增 `durationMs` 字段，调工具时记开始和结束时间算出来再返回；单次诊断的 step 里本来就有 `durationMs`，直接用。工具极快导致耗时为 0 时前端显示“—”，不编一个数字。字段不用 omitempty，0 本身就是一个明确的值。
- **滚动控制**（`frontend/src/App.tsx`）：聊天只有用户已经在最底下时才跟着滚到底，否则显示“↓ 有新消息”按钮，不把正在往上翻历史的用户拽下来。
- **详情结果全部回聊天区，按类型渲染**（`frontend/src/types.ts` 的 `buildBlocks`、`frontend/src/components.tsx` 的 `ResultBlockView`）：后端返回的结构化结果先按 key 映射成带类型的结果块再渲染，类型有 overview（集群总览卡片）、podList（Pod 表格）、deploymentList（Deployment 表格）、events（事件时间线）、report（诊断报告卡）、repairSuggestion（修复建议卡）、text（纯文本）。工具名原样当 key 的原始结果（如 get_pod）是 canonical key 的重复，不再单独成块，避免同一份数据出现两次。不知道的类型不渲染，不白屏。每个结果块默认折叠一部分：Pod 表格先显示前 10 行，点开看全部；事件先显示前 5 条，点开看全部；修复建议卡只显示一行摘要，点开看详情。审批卡、修复执行和验证结果也在聊天区作为结果块出现：点了“转成正式诊断”后留在对话模式，审批卡直接出现在聊天里，不用再去右侧找。
- **修复决策只走模型单一路**（`internal/chat/real.go`、`internal/run/worker.go`、`internal/model/model.go`）：真模型路径去掉 `synthesizeRepair` 兜底。模型给了最终回答但没有结构化修复建议时，不再由后端按 OOM 证据合成一张卡，而是再问模型一次，让它用 `submit_diagnosis` 把 `suggestedRepair` 补回来；补不回来就老实说还没法下结论、需要更多证据，不要编一张卡。正式诊断的 Worker 也一样：模型报告里没给新值就不生成提案，不再按旧值翻倍兜底。系统提示词（对话和单次诊断两处）都加了一句：用户明确说了增加量（例如“增加 128Mi”、“调到 256Mi”）时，模型按用户说的算新值，不要默认翻倍；解析用户的话是模型的事，后端不做正则解析，只校验模型算出来的数。桩模型（Stub）的 `synthesizeRepair` 调用留着，但明确标注只是测试路径的确定性规则，不是产品逻辑；离线评估（`go run ./cmd/eval`）走桩模型，仍 3/3 通过。
- **修复建议卡丢失 bug 修复**（`internal/chat/chat.go` 的 `RunMessage`）：根因是保存助手消息时只存了 ToolCalls 和 Structured，没存 SuggestedRepair；runStub 返回的建议只在 Reply 顶层，前端重新拉会话时就丢了，按钮也没了。现在统一在 `RunMessage` 里做：只要本轮给了建议，就写进 `Structured["suggestedRepair"]`，stub 和 real 两条路都走同一个保存逻辑。runStub 里也同步把建议写进 Structured，两边一致。前端的“转成正式诊断”按钮只要有建议卡就一直显示，不只在最新一条消息上才显示，翻历史、刷新后按钮还在。安全流程不变：聊天里不给直接“执行”按钮，修复还是要走“转成正式诊断 → 人工审批 → 执行验证”那条线，不绕过审批。
- **回归**：`go test ./...` 全部通过（含新增的建议卡持久化、工具耗时、真模型无合成卡、Worker 缺新值不生成提案四个测试），`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、转正式诊断后审批执行验证全部回归通过。

## v0.5.0（2026-11-20）

### 改了什么

- **完整 React 前端**（`frontend/`，Vite + React + TypeScript，不引入 UI 框架）：左侧会话列表（新建会话、会话历史），中间聊天区和单次诊断，右侧详情区（当前任务的报告、审批卡、修复验证）。聊天区里助手消息内嵌工具执行时间线：每一步显示工具名、参数摘要、状态（成功 / 失败）、结果摘要，正在执行的工具带转圈和“正在执行”提示。结构化结果渲染成卡片和表格：集群总览是数字卡片，Pod 列表和 Deployment 列表是表格（系统行带“系统”徽章），事件是时间线，修复建议卡、审批卡、执行/验证结果分开显示。单次诊断页保留 v0.3.0 的全部功能（提交、轮询时间线、报告、审批、执行验证），只是用 React 重写。空状态和错误状态都有文案，不会白屏。
- **工具执行实时显示用轮询，不用 WebSocket**：对话的 POST 在服务端跑整轮，期间 tool 消息会陆续落盘，前端每 800ms 轮询一次会话，就能实时看到正在执行哪个工具；单次诊断每 900ms 轮询 `GET /api/runs/{id}/events?after=N` 追加时间线，同时刷新任务快照。轮询够用，后端不用为实时显示改协议。
- **前后端同仓库、单二进制部署**：React 源码在 `frontend/`，构建产物输出到 `web/dist/`，Go 用 `go:embed` 把 dist 打进二进制。交付 zip 里自带构建好的 dist，解压后不用装 Node，直接 `go run ./cmd/server` 就能看到 React 页；想改前端时再按 `frontend/README.md` 装 Node 重建。`make frontend` 是装依赖 + 构建，`make run` 是直接启动（dist 已包含），`make run-frontend` 是先构建前端再启动。
- **系统命名空间读放开、写挡住**：去掉发现工具（`internal/run/tools.go`）对 kube-system / kube-public / kube-node-lease 的过滤，`list_namespaces`、`list_deployments`、`list_problem_pods`、`get_cluster_overview` 都能返回系统命名空间的结果，行里带 `isSystem: true` 标记，总览里单独列出系统命名空间数和系统有问题 Pod 数；对话的系统提示词改成“可以查系统命名空间做诊断，但不要把它当成用户业务下结论，不要给系统组件出修复建议”；`internal/agent.ValidateToolCall` 和 `POST /api/runs` 的创建校验也不再拒绝系统命名空间（诊断是只读）。写操作继续严格挡住：`internal/repair.ValidatePatchSpec` 对系统命名空间的拒绝原样保留，RBAC 清单不动。
- **回归**：`go test ./...` 全部通过，`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、修复建议卡 128Mi→256Mi、对话只读、转正式诊断后审批执行验证全部回归通过。

## v0.4.0（2026-11-20）

### 改了什么

- **通用 Agent**（`internal/agent/`、`internal/model/`、`internal/run/worker.go`）：报告里新增可选的 `suggestedRepair` 字段（目标 namespace / Deployment / 容器、字段、旧值、新值、理由），字段目前只支持 `memoryLimit`，别的字段会被校验层清掉。Stub 的修复建议从已查到的 Deployment 结果里算出来（容器名、旧值来自证据，新值是旧值翻倍封顶 4Gi），不再写死 demo-api / 128Mi；真模型的 `submit_diagnosis` 也加了 `suggestedRepair` 字段，系统提示词里明确“不要预设故障类型，按证据自己判断”。
- **通用修复白名单**（`internal/repair.ValidatePatchSpec`）：任意 namespace / Deployment / 容器都可以提修复建议，但只允许改内存 limit，值必须是 Mi / Gi 结尾，新值必须大于旧值、不超过旧值的 4 倍、不超过 4Gi，系统命名空间一律拒绝。旧值和集群现状是否一致仍由执行前校验重新读目标时核对（旧值不匹配标为过期，不 Patch）。
- **通用提案生成**（`internal/run/worker.go`）：Worker 不再看原因里有没有 OOM，而是看报告里有没有 `suggestedRepair`；容器名没给时读 Deployment 的第一个容器，旧值以集群现状为准（建议里的旧值和现状不一致就放弃），新值必须由模型给，没给就不生成提案（v0.6.0 去掉了以前“新值缺失时按旧值翻倍”的兜底，修复决策只能由模型做）。校验不通过就不生成提案，诊断报告本身仍然完整。
- **KUBE_MODE 通用化**（`cmd/server/main.go`）：`fake | kubeconfig`，`kind` 是 `kubeconfig` 的别名，做兼容保留。kind、k3s、云厂商集群都读 kubeconfig，不再为每种集群写一套客户端；k3s 的 kubeconfig 在 `/etc/rancher/k3s/k3s.yaml`，用 `KUBECONFIG` 指过去即可。
- **发现工具**（`internal/run/tools.go`、`internal/kube/client.go`）：新增 4 个只读工具，注册进 Agent 工具表，模型在对话里能自主调用：`list_namespaces`、`list_deployments`（namespace 为空查全部）、`list_problem_pods`（非 Running、未 Ready、重启过、有等待原因）、`get_cluster_overview`（节点数、namespace 数、Deployment 数、Pod 数、有问题 Pod 数）。返回的都是结构化 JSON，前端渲染成表格和卡片。注意：v0.4.0 时系统命名空间的结果会被过滤掉，v0.5.0 已改成读放开、写挡住，系统行带 `isSystem: true` 标记，见 v0.5.0。
- **对话会话**（`internal/chat/`、`internal/store/`）：SQLite 新增 `conversations` 和 `messages` 两张表（role 分 user / assistant / tool）。新 API：`POST /api/conversations`、`GET /api/conversations`、`GET /api/conversations/{id}`、`POST /api/conversations/{id}/messages`、`POST /api/conversations/{id}/convert`。每次调用模型带最近 10 条消息，超出截断，数据库里全量保留；TODO 是以后对 10 条之前的历史做摘要，现在不做假的摘要。指代消解靠历史实现：系统提示词写清“这个 ns / 刚才那个 Pod 要按最近提到的目标理解，理解不了就主动问，不要猜”。
- **对话只读**：对话里模型只能调只读工具，不能直接 Patch；需要修复时返回结构化修复建议卡，不生成 `change_requests`。用户点“转成正式诊断”后，才走现有的 `/api/runs` 流程，生成提案、走人工审批、再执行和验证。
- **Runbook 扩到 8 篇**：新增 CrashLoopBackOff、Pending 调度失败、Service 无 Endpoints、ConfigMap/Secret 缺失、资源配额不足 5 篇，每篇都有中文关键词和别名，FTS5 索引能命中；原有 3 篇没删，`go run ./cmd/eval` 仍 3/3 通过。

### 已实现

- 后端通用化：报告里的 `suggestedRepair`、通用白名单（任意目标、只改内存 limit、新值大于旧值、不超过 4 倍、不超过 4Gi）、通用提案生成（容器名、旧值、新值从集群现状补齐）、`KUBE_MODE=kubeconfig`（`kind` 是别名）
- 发现工具：`list_namespaces`、`list_deployments`、`list_problem_pods`、`get_cluster_overview`，全部只读，结构化返回
- 对话模式：会话 + 消息持久化、最近 10 条历史截断、指代消解、对话只读、修复建议卡、转正式诊断
- Runbook 扩到 8 篇，评估仍 3/3 通过；单次诊断、审批执行、rollout 验证全部回归通过

### 未实现（当时未做，v0.5.0 已实现部分）

- 完整 React + TypeScript 前端（v0.5.0 已做，见上文）
- 系统命名空间读放开（v0.5.0 已做：读能查、写仍拒绝）
- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控

## v0.3.0（2026-11-20）

### 改了什么

- **修复执行**（`internal/repair/`）：Worker 领取已批准的提案后，先重新读取目标，核对 UID、容器名、变更前旧值，再 dry-run，再用本次读取的 resourceVersion 做条件 Patch，只改指定 Deployment、指定容器的内存 limit 这一个字段，值只允许是提案里的新值。写入后读回确认，写入超时或中断时先读回目标再下结论，归因不明确记 unknown，不谎报成功。前提变了（UID 变了、旧值变了、容器没了）标为过期，不执行；当前值已是新值时直接记为已执行，不重复 Patch。
- **白名单**（`internal/repair.ValidatePatchSpec`）：只允许 demo/demo-api/demo-api 这一组目标，内存值必须是 Mi 结尾，新旧值不能相同，其他一律拒绝。RBAC 只能限到资源和动词，改哪个字段是代码白名单在管，两层都要有。
- **写权限分离**：诊断走 `kube.KubeClient`（只读），修复走新增的 `kube.RepairClient`（PatchDeploymentMemoryLimit 等），Fake 和 Real 各实现一份。本地 kind 演示仍用同一个 kubeconfig，但对象是分开建的；生产上把 `REPAIR_KUBECONFIG` 指向写身份专用的 kubeconfig 即可，修复代码不用改。
- **rollout 验证**（`internal/repair/`）：独立检查 observedGeneration 追上 generation、updatedReplicas 和 availableReplicas 达标、新 Pod Ready、验证窗口内重启次数没有继续增长。新 Pod 按 pod-template-hash 区分，不把旧 Pod 的 Ready 当成新 Pod 的。超时 120 秒（默认），到点标 timeout，不一直循环；连续两轮 Ready 才算通过，避免只看一眼就放行。执行和验证结果分开存、分开展示，写入成功但验证失败时前端能分别看到。
- **Fake 模拟 rollout**：fake clientset 没有 Deployment 控制器，Patch 后不会自动建新 Pod，所以 Fake 的 Patch 在写完后模拟控制器行为：generation +1、status 置为已更新且可用、建带新 hash 的 ReplicaSet 和一个 Ready 的新 Pod。只在 Fake 里模拟，Real 不走这条路。
- **Worker 接入**：诊断没有任务时，Worker 轮询已批准的提案并执行验证。诊断和修复状态分开，诊断报告能独立于修复状态展示。等待审批的提案一直存在数据库里，重启后仍能领取；执行中途崩掉的提案重启后改回待执行，重跑是安全的（执行前校验先看当前值）。
- **提案生成放宽**：原来只在 high 置信度时生成提案，真模型在 kind 上跑 OOM 常常只给 medium（日志为空、没有实时 metrics，它不敢说 high），但 OOM 本身是直接原因，调高 limit 是低风险、可逆的操作，而且还要经过人工审批，所以 high 或 medium 都生成，low 仍不生成。
- **前端静态页**（`web/index.html`，仍是静态页，不是 React）：加了审批卡（目标、变更前后值、相关证据，批准/拒绝按钮）和修复展示（执行结果和验证结果分开显示、rollout 状态、新 Pod、重启次数）。无构建步骤，Go 服务同源托管。
- **RBAC 清单**（`scripts/kind/rbac.yaml`）：只读身份和受写身份的参考清单，写身份只能 patch demo-api 这一个 Deployment（resourceNames 限定）。
- **修复了两个 v0.2.0 遗留问题**：Agent 工具失败不回传给模型的问题在 v0.2.0 已修，本次未改坏；另外本次发现并修了一个相关问题——提案生成时没有绑定 UID，执行前无法校验目标是否被重建，现在生成时绑定 UID，执行前核对。

### 已实现

- 修复执行：条件 Patch、dry-run、写入后读回确认、超时/中断后读回再下结论、过期和幂等处理
- rollout 验证：generation、副本数、新 Pod Ready、重启次数不增长，四项都达标且连续两轮才通过，超时标 timeout
- Worker 接入：批准后自动执行并验证，拒绝后无写入，重启后仍能领取
- 前端审批卡和修复展示（静态页），RBAC 参考清单
- 提案生成放宽到 high 或 medium，真模型 medium 也能出提案

## v0.2.0（2026-11-20）

### 改了什么

- **真模型适配器**（`internal/model/`）：OpenAI 兼容的 chat completions + tools 协议，不写死厂商。模型返回的 tool_calls 映射成 `agent.ToolCall`，`submit_diagnosis` 映射成最终报告；非法工具名原样交给 Agent 的 `ValidateToolCall` 拒绝，不崩循环。超时、重试（指数退避，最多 3 次，只重试 429 / 5xx / 网络错误）、上下文截断（只传最近 6 步摘要，每条最多 500 字，全量证据仍在 SQLite 里）。
- **知识库升级**（`internal/knowledge/`）：`runbooks/*.md` 在启动时建成 SQLite FTS5 索引，`search_runbooks` 改用 FTS5 按相关性排序，返回片段和来源。注意一个坑：FTS5 的 unicode61 会把一整句中文当成一个长词，查“内存”查不到，所以表里多了一个 keywords 列，存空格分开的别名（OOM、内存、探针……），短词查询才能命中。FTS5 未启用、出错或查不到时回退到 v0.1.0 的关键词检索。
- **离线评估扩充**（`cmd/eval/` + `testdata/cases/`）：除 OOM 外新增镜像拉取失败、探针失败两个用例，每种用例有自己的 fake 场景数据（Pod / Events / 日志）。输出每个用例的通过与否、命中率、证据引用准确性、工具步数、耗时汇总。`MODEL_MODE=real` 但无 Key 时跳过真模型评估并明确提示，不报错退出。
- **kind 真集群演示脚本**（`scripts/kind/`）：`demo-api.yaml` 用公开的 `python:3.12-slim` 镜像不断分配内存，真实触发 OOM，不需要自己构建镜像；`setup-demo.sh` 一键建 namespace、应用 Deployment、等到 OOMKilled 或重启次数达标，并打印手动验证命令。
- **Stub 扩展**：桩模型从只会 OOM 扩展到 OOM / 镜像 / 探针三类场景，仍是确定性剧本，只为让 eval 三个用例在 stub 模式下跑通。修了一个 v0.2.0 发现的 bug：探针案曾被 search_runbooks 带回的镜像手册带偏，误判成镜像拉取；现在 Stub 先看 Pod / Events / 日志这些一手证据，再看检索结果。
- **评估命中口径收紧**：命中只看 Cause，不看 Facts。之前只看 Facts 会把错因判成通过，掩盖误判。

### 已实现

- 真模型适配器：OpenAI 兼容 chat completions + tools，DeepSeek / OpenAI 只换环境变量；超时、重试、上下文截断；非法工具名交给 Agent 校验层拒绝
- FTS5 索引：启动时从 `runbooks/*.md` 建索引，按相关性排序，查不到回退到关键词检索
- 离线评估：三个用例在 stub 模式下 3/3 通过，输出命中率、引用准确性、步数、耗时；真模型无 Key 时跳过
- kind 演示脚本：公开镜像真实 OOM 的 Deployment + 一键 setup 脚本 + 手动验证步骤
- fake 客户端三类场景：OOM、镜像拉取失败、探针失败
- v0.1.0 回归：fake + stub 全流程仍完成并生成提案

## v0.1.0（2026-11-20）

### 改了什么

- 后端骨架：Go HTTP API + SQLite（tasks、tool steps、evidence、approvals）+ 后台 Worker（领取任务、按已提交步骤恢复），单进程运行
- Agent 循环：最多 8 步、60 秒超时，6 个只读工具（get_pod、list_workload_pods、get_events、get_pod_logs、get_deployment、search_runbooks），`KUBE_MODE=fake|kubeconfig`、`MODEL_MODE=stub|real` 可切换
- 诊断只读，修复需人工审批：审批只记一条记录，Worker 轮询已批准提案
- fake 客户端预置 demo/demo-api OOM 场景，桩模型确定性走完诊断流程
- 最小静态前端 `web/index.html`，提交任务、轮询时间线、查看报告
