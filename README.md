# KubeCause — Kubernetes 故障诊断 Agent

当前版本：v0.13.0。更新记录见 [CHANGELOG.md](CHANGELOG.md)。

KubeCause 是一个 Kubernetes 故障诊断 Agent。用户选定 namespace 和 Pod 或 Deployment，描述故障现象，Agent 自主选择只读工具收集证据，检索排障手册，输出诊断结论和建议。演示环境支持人工审批后的变更：用一组通用 Patch 改指定 Deployment 的白名单字段（镜像、内存 limit/request、CPU limit/request、副本数），并验证变更后的运行状态；也可以在对话里直接创建一个新的 Deployment，同样走建议卡 → 人工审批 → 执行 → 验证 rollout 这条线。

## 功能

- **对话诊断**：选定 namespace 和 Pod 或 Deployment，描述故障现象，Agent 自主选择只读工具收集证据，检索排障手册（`runbooks/*.md`，SQLite FTS5 索引），输出诊断结论和修复建议。支持多轮追问，指代消解靠最近 10 条用户/助手历史消息（工具调用不占名额）。
- **单次诊断**：`POST /api/runs` 创建一个有 runId 的正式诊断任务，有步骤时间线、诊断报告、提案、审批、执行和验证，全程可审计，重启后可恢复。
- **通用 Patch 修复**：修复建议是一组通用 Patch（`{op, path, value}`），只改白名单字段（镜像、内存/CPU 的 limit/request、副本数），不按故障类型分分支。人工审批后才执行，执行前 dry-run、核对 resourceVersion，执行后逐条读回确认，再做 rollout 验证。
- **创建 Deployment**：对话里说“在 demo 空间下创建一个 Deployment，镜像 nginx:1.25”，Agent 先问缺的信息（命名空间、名字、镜像、副本数），问全了给一张创建建议卡，点“创建可审批的创建任务”后走同一套审批 → 执行 → 验证 rollout 的线。只创建 Deployment，不直接创建裸 Pod（Pod 挂了不会自动重建，也没有 rollout）；创建校验和 Patch 共用同一套安全思路（系统命名空间拒绝、镜像合法、副本数 1–10、内存/CPU 在上限内）。
- **转换带目标**：建议卡上的转换按钮把这张卡的目标（namespace/kind/name）和 Patches 一起传给后端，Worker 直接拿建议校验、生成提案，不用重新诊断一遍。以前只传会话 id，后端去历史里猜目标，会猜错（demo-bad-image 的卡转出来却是 demo-api 的任务），重跑 8 步耗尽、置信度 low、没有提案。
- **发现工具**：`list_namespaces`、`list_deployments`、`list_problem_pods`、`get_cluster_overview`，全部只读，结构化返回，前端渲染成卡片和表格。系统命名空间可查不可改。
- **配置中心**：`config.yaml` + 环境变量覆盖 + `GET/POST /api/config` 热加载，前端“设置”页能改，不用重启。`MODEL_API_KEY` 只从环境变量读，绝不进配置。
- **离线评估**：`go run ./cmd/eval` 跑 OOM、镜像、探针三个用例，输出命中率、引用准确性、步数和耗时。
- **工具调用只存一份（round_id 关联）**：一次工具调用在库里只存在 tool 消息里，每调一次一条；同一轮的用户 / tool / 助手消息共享一个 `round_` 开头的 roundID，助手消息不再抄一份 tool_calls，前端按 roundId 关联数“查了 N 个工具”，右侧活动面板只读 tool 消息，不用去重。以前助手消息里抄了一份，前端按“消息 id + 调用 id”去重认不出来，同一条调用显示两遍（耗时毫秒数完全相同）。老数据没有 round_id，读出来是空，前端回退到位置推导（上一条用户消息到本条助手消息之间的 tool 消息），再没有就读助手消息里存的老副本。
- **模型请求调试日志**：`KUBE_DEBUG_MODEL=1` 启动时，对话里每次调模型前把实际发过去的完整消息数组（system + 历史 + 当前问题 + 本轮的 assistant(tool_calls) / tool 往返）打到服务端 stdout，返回后再打一条返回日志（调了哪个工具及参数，或最终文本 / 是否提交了诊断报告）。默认关闭，页面上没有任何调试痕迹，API 回复里也不含调试数据；日志里不含 API Key（Key 只在请求头里）。单条内容超长时截断并注明原长。
- **不再追问结构化建议**：模型给最终文本但没交诊断报告时，直接显示文本，不再追加假 user 消息追问一次；交不交报告由模型自己决定（系统提示词里关于 submit_diagnosis 的指引保留，那只是指引）。模型一次返回里的文字和工具调用 / 报告都保留：文字在前、格式化报告在后，调工具时顺带的文字也拼进回复，不再悄悄替换或丢掉。执行动作（修复提议怎么落地、按钮、审批流）的重设计是下一步，不在这版。

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

# 创建 Deployment：不用先诊断，缺信息先问
# 第一轮：问“在 demo 空间下创建一个 Deployment，镜像 nginx:1.25”
#   → Agent 问“还缺名字”
# 第二轮：回“叫 web-demo”
#   → Agent 给一张创建建议卡，点“创建可审批的创建任务”走审批执行
# 说“创建一个 Pod”时也按 Deployment 给卡，并说明裸 Pod 不会自动重建
```

## 创建提案的校验

创建只针对 Deployment，不直接创建裸 Pod。校验不过不生成提案：系统命名空间（kube-system / kube-public / kube-node-lease）拒绝；镜像名必须合法；副本数 1–10（没说默认 1）；内存 limit/request 有值时必须是 Mi/Gi 结尾且不超过 `repair.maxMemoryMi`（默认 4Gi，配置中心可调），request 不超过同类 limit；CPU limit/request 有值时必须是合法数量且大于 0。执行前再查一次目标不存在才能创建：已存在且配置一致视为已创建（幂等），已存在且配置不一致标过期，不覆盖别人的 Deployment。执行后验证和 Patch 提案共用同一套 rollout 验证（observedGeneration、新 Pod Ready、重启次数不增长）。

## 配置中心

`config.yaml`（仓库根目录）+ 环境变量覆盖 + `GET/POST /api/config` 热加载。启动时先读文件，没有文件用默认值；环境变量优先于文件；前端“设置”页改完点保存，立即生效，不用重启。

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `chat.maxSteps` | 12 | 对话最大工具步数：一轮对话里模型最多调几次工具。预算，不是决策——调大不干预模型怎么选工具，只是允许查得更多；步数用完返回半成品，不报错 |
| `chat.maxHistoryMessages` | 10 | 每次调用模型时带上的最近用户/助手消息条数，只算 user 和 assistant，tool 消息不占名额；更早的消息数据库里全量保留 |
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

## 演示案例

两个案例都在 `demo` 命名空间，用来验证 Agent 的通用发现能力，代码里对它们没有任何特殊处理。

### OOM（demo-api）

Deployment `demo-api`，镜像 `python:3.12-slim`，容器不断分配内存（每 0.05 秒分配 1MiB，永不释放），真实触发 OOMKilled。资源 requests 64Mi/100m、limits 128Mi/500m。部署见下文“kind 演示脚本”。预期：Pod 反复重启，事件里有 OOMKilled，Agent 应给出内存 limit 128Mi → 256Mi 的 Patch 建议。注意：调大 limit 只是缓解，不是根治泄漏，验证时重启次数增长会标为失败，这是符合预期的。

### ImagePullBackOff（demo-bad-image）

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
| `KUBE_DEBUG_MODEL` | 空 | 设为 `1` 时打开模型请求调试：对话里每次调模型的完整请求和返回打到服务端 stdout；默认关闭，页面上无任何调试痕迹 |

## 模型请求调试

想看看到底发给了模型什么东西（历史怎么拼的、当前问题在不在最后一条、本轮调了哪些工具），不用在页面上找：发给模型是后端 Go 服务直接调模型 API，浏览器碰不到模型，所以调试只打服务端日志。

```bash
KUBE_DEBUG_MODEL=1 go run ./cmd/server
```

打开后，每次调模型前打一条请求日志、返回后打一条返回日志，前缀都是 `[debug model]`，带会话 id、round id 和第几次请求：

```
[debug model] conv=conv_... round=round_... step=1 request=[{"role":"system",...},{"role":"user","content":"当前状态怎么样"}]
[debug model] conv=conv_... round=round_... step=1 response tool=get_cluster_overview args={}
```

日志里是实际发过去的完整数组，不是事后重建的摘要；单条内容超长时截断并注明原长，角色、顺序、tool_calls 不丢。API Key 在请求头里，不在数组里，日志里不会出现。桩模型不调外部模型，不打这个日志。默认关闭时一行都不打，API 回复里也没有调试字段。

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

注意：本机（Muse 的 Linux VM）没有 Docker / kind，无法做 kind 端到端验证，脚本和上面的步骤就是给 Mac 上手动验证准备的。`KUBE_MODE=kubeconfig` 的 client-go 读 kubeconfig 逻辑已在 v0.1.0 实现，v0.2.0 没有改坏它，v0.4.0 只是把 `kind` 改成了别名。

## 前端（React）

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
| `scripts/kind/` | kind 演示：demo-api.yaml 和 setup-demo.sh（OOM 案例）、demo-bad-image.yaml 和 setup-bad-image.sh（ImagePullBackOff 案例） |

## 架构文档

完整架构见 `ARCHITECTURE.md`（仓库外另存），架构图见 `docs/diagrams/architecture.png`。

## 更新记录

每个版本的改动见 [CHANGELOG.md](CHANGELOG.md)，按版本号倒序，v0.9.0 是当前版本。
