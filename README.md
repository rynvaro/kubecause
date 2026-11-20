# KubeCause — Kubernetes 故障诊断 Agent（Slice 5）

KubeCause 是一个 Kubernetes 故障诊断 Agent。用户选定 namespace 和 Pod 或 Deployment，描述故障现象，Agent 自主选择只读工具收集证据，检索排障手册，输出诊断结论和建议。演示环境支持一类人工审批后的变更，即调整指定 Deployment 的容器内存限制，并验证变更后的运行状态。

Slice 5 在 Slice 4 的基础上做了两件事：**完整 React 前端**和**系统命名空间读放开**。前端用 Vite + React + TypeScript 重写，左边会话列表、中间聊天、右边详情，工具执行过程用轮询实时显示成时间线，结构化结果渲染成卡片和表格，不再贴 JSON；构建产物用 `go:embed` 打进 Go 二进制，部署还是一个进程。系统命名空间改成读放开、写挡住：发现工具和对话都能查 kube-system，行里打“系统”标记，修复校验仍然拒绝系统命名空间。

Slice 1/2/3/4 已有的东西没有重写：SQLite 表、HTTP 接口、Worker 状态机、Agent 循环的校验和恢复、fake 客户端、桩模型、真模型适配器、FTS5 检索、条件 Patch 与 rollout 验证、对话会话、发现工具，全部保留并做了回归。

## Slice 5 改了什么

- **完整 React 前端**（`frontend/`，Vite + React + TypeScript，不引入 UI 框架）：左侧会话列表（新建会话、会话历史），中间聊天区和单次诊断，右侧详情区（当前任务的报告、审批卡、修复验证）。聊天区里助手消息内嵌工具执行时间线：每一步显示工具名、参数摘要、状态（成功 / 失败）、结果摘要，正在执行的工具带转圈和“正在执行”提示。结构化结果渲染成卡片和表格：集群总览是数字卡片，Pod 列表和 Deployment 列表是表格（系统行带“系统”徽章），事件是时间线，修复建议卡、审批卡、执行/验证结果分开显示。单次诊断页保留 Slice 3 的全部功能（提交、轮询时间线、报告、审批、执行验证），只是用 React 重写。空状态和错误状态都有文案，不会白屏。
- **工具执行实时显示用轮询，不用 WebSocket**：对话的 POST 在服务端跑整轮，期间 tool 消息会陆续落盘，前端每 800ms 轮询一次会话，就能实时看到正在执行哪个工具；单次诊断每 900ms 轮询 `GET /api/runs/{id}/events?after=N` 追加时间线，同时刷新任务快照。轮询够用，后端不用为实时显示改协议。
- **前后端同仓库、单二进制部署**：React 源码在 `frontend/`，构建产物输出到 `web/dist/`，Go 用 `go:embed` 把 dist 打进二进制。交付 zip 里自带构建好的 dist，解压后不用装 Node，直接 `go run ./cmd/server` 就能看到 React 页；想改前端时再按 `frontend/README.md` 装 Node 重建。`make frontend` 是装依赖 + 构建，`make run` 是直接启动（dist 已包含），`make run-frontend` 是先构建前端再启动。
- **系统命名空间读放开、写挡住**：去掉发现工具（`internal/run/tools.go`）对 kube-system / kube-public / kube-node-lease 的过滤，`list_namespaces`、`list_deployments`、`list_problem_pods`、`get_cluster_overview` 都能返回系统命名空间的结果，行里带 `isSystem: true` 标记，总览里单独列出系统命名空间数和系统有问题 Pod 数；对话的系统提示词改成“可以查系统命名空间做诊断，但不要把它当成用户业务下结论，不要给系统组件出修复建议”；`internal/agent.ValidateToolCall` 和 `POST /api/runs` 的创建校验也不再拒绝系统命名空间（诊断是只读）。写操作继续严格挡住：`internal/repair.ValidatePatchSpec` 对系统命名空间的拒绝原样保留，RBAC 清单不动。
- **回归**：`go test ./...` 全部通过，`go run ./cmd/eval` 仍 3/3 通过；对话两轮能接上文、修复建议卡 128Mi→256Mi、对话只读、转正式诊断后审批执行验证全部回归通过。

## Slice 4 改了什么

- **通用 Agent**（`internal/agent/`、`internal/model/`、`internal/run/worker.go`）：报告里新增可选的 `suggestedRepair` 字段（目标 namespace / Deployment / 容器、字段、旧值、新值、理由），字段目前只支持 `memoryLimit`，别的字段会被校验层清掉。Stub 的修复建议从已查到的 Deployment 结果里算出来（容器名、旧值来自证据，新值是旧值翻倍封顶 4Gi），不再写死 demo-api / 128Mi；真模型的 `submit_diagnosis` 也加了 `suggestedRepair` 字段，系统提示词里明确“不要预设故障类型，按证据自己判断”。
- **通用修复白名单**（`internal/repair.ValidatePatchSpec`）：任意 namespace / Deployment / 容器都可以提修复建议，但只允许改内存 limit，值必须是 Mi / Gi 结尾，新值必须大于旧值、不超过旧值的 4 倍、不超过 4Gi，系统命名空间一律拒绝。旧值和集群现状是否一致仍由执行前校验重新读目标时核对（旧值不匹配标为过期，不 Patch）。
- **通用提案生成**（`internal/run/worker.go`）：Worker 不再看原因里有没有 OOM，而是看报告里有没有 `suggestedRepair`；容器名没给时读 Deployment 的第一个容器，旧值以集群现状为准（建议里的旧值和现状不一致就放弃），新值没给时按旧值翻倍。校验不通过就不生成提案，诊断报告本身仍然完整。
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
| `scripts/kind/` | kind 演示：demo-api.yaml 和 setup-demo.sh |

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

## Slice 4 未实现（Slice 5 已实现）

- 完整 React + TypeScript 前端（Slice 5 已做，见上文）
- 系统命名空间读放开（Slice 5 已做：读能查、写仍拒绝）
- 对话历史摘要（现在是截断，TODO 在代码和 README 里写清了）
- 云端部署、鉴权、监控

## 架构文档

完整架构见 `ARCHITECTURE.md`（仓库外另存），架构图见 `docs/diagrams/architecture.png`。
