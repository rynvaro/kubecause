# KubeCause — Kubernetes 故障诊断 Agent（Slice 3）

KubeCause 是一个 Kubernetes 故障诊断 Agent。用户选定 namespace 和 Pod 或 Deployment，描述故障现象，Agent 自主选择只读工具收集证据，检索排障手册，输出诊断结论和建议。演示环境支持一类人工审批后的变更，即调整指定 Deployment 的容器内存限制，并验证变更后的运行状态。

Slice 3 在 Slice 2 的基础上，把修复真正做出来了：**审批后执行条件 Patch、rollout 验证、静态页审批卡和修复展示、RBAC 参考清单**。完整 React 前端仍未做，仍是静态页。

Slice 1/2 已有的东西没有重写：SQLite 三张表、四个 HTTP 接口、Worker 状态机、Agent 循环的校验和恢复、fake 客户端、桩模型、真模型适配器、FTS5 检索，全部保留并做了回归。

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
| `KUBE_MODE` | `fake` | `fake` 用 client-go fake clientset，预置 demo-api 数据；`kind` 用真 client-go 读 kubeconfig |
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

### kind + stub（Mac 上跑真集群，桩模型）

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

然后启动服务：

```bash
KUBE_MODE=kind MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
```

找不到 kubeconfig 时启动会明确报错，请确认已执行 `kind create cluster`，且 `~/.kube/config` 存在，或用 `KUBECONFIG` 指定。

### kind + 真模型（Mac 上，DeepSeek 示例）

```bash
# Key 只从环境变量读，不要写进文件、不要提交
export MODEL_API_KEY="sk-..."
KUBE_MODE=kind MODEL_MODE=real MODEL_BASE_URL=https://api.deepseek.com/v1 MODEL_NAME=deepseek-chat DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
```

换 OpenAI 时只改两个变量：

```bash
export MODEL_API_KEY="sk-..."
KUBE_MODE=kind MODEL_MODE=real MODEL_BASE_URL=https://api.openai.com/v1 MODEL_NAME=gpt-4o-mini DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
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

注意：本机（Muse 的 Linux VM）没有 Docker / kind，无法做 kind 端到端验证，脚本和上面的步骤就是给 Mac 上手动验证准备的。`KUBE_MODE=kind` 的 client-go 读 kubeconfig 逻辑已在 Slice 1 实现，Slice 2 没有改坏它。

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
| `internal/agent/` | Agent 循环：步数上限、时限、工具校验、证据引用校验 |
| `internal/model/` | 模型适配器：stub 桩模型（三类场景确定性剧本），real 是 OpenAI 兼容协议的真模型 |
| `internal/kube/` | client-go 只读访问：fake 预置三类场景数据，kind 读 kubeconfig |
| `internal/knowledge/` | 排障手册检索：FTS5 索引（keywords 列解决中文短词查询）+ 关键词回退 |
| `internal/repair/` | 修复执行：白名单校验、条件 Patch、dry-run、写入后读回确认、rollout 验证（执行和验证分开存） |
| `scripts/kind/rbac.yaml` | RBAC 参考清单：只读身份和受写身份（写身份只能 patch demo-api） |
| `runbooks/` | 排障手册 Markdown |
| `testdata/cases/` | 评估用例：OOM、镜像拉取失败、探针失败 |
| `scripts/kind/` | kind 演示：demo-api.yaml 和 setup-demo.sh |
| `web/` | 极简静态演示页，由 Go 服务同源托管 |

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

## Slice 3 未实现

- 完整 React + TypeScript 前端（目前是静态页，审批卡和修复展示已做）
- 多集群、多 namespace 演示、鉴权、监控

## 架构文档

完整架构见 `ARCHITECTURE.md`（仓库外另存），架构图见 `docs/diagrams/architecture.png`。
