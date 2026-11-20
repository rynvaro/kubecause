# KubeCause — Kubernetes 故障诊断 Agent（Slice 1）

KubeCause 是一个 Kubernetes 故障诊断 Agent。用户选定 namespace 和 Pod 或 Deployment，描述故障现象，Agent 自主选择只读工具收集证据，检索排障手册，输出诊断结论和建议。演示环境支持一类人工审批后的变更，即调整指定 Deployment 的容器内存限制，并验证变更后的运行状态。

Slice 1 只做最小闭环：**创建任务、工具调用、时间线、诊断报告**，用固定工具响应和桩模型跑通。真模型、真修复、完整 React 前端都在后面的切片。

## 先决条件

- Go 1.23 或更新版本
- Mac 上跑真集群演示需要：Docker Desktop、kind、kubectl（见下文 kind 步骤）
- 本机测试不需要集群，`KUBE_MODE=fake` 就够了

## 安装

```bash
git clone <仓库地址>
cd kubecause
go mod download
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
make eval   # 离线评估，跑 testdata/cases/oom.json
```

## 启动

### fake 模式（默认，不需要集群）

```bash
KUBE_MODE=fake MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
```

打开 http://localhost:8080 ，提交 `demo / Deployment / demo-api / demo-api 为什么一直重启`，就能看到工具步骤、时间线和诊断报告。

### kind 模式（Mac 上跑真集群，桩模型）

先在 Mac 上装 kind：

```bash
# 1. 装 Docker Desktop，启动后确认 docker ps 能跑
#    设置里内存调到 4G 以上，CPU 2 核以上
# 2. 装 kind 和 kubectl
brew install kind kubectl
kind version
kubectl version --client
# 3. 建集群（配置文件见仓库外的 kubecause-kind-config.yaml，或直接用下面这句）
kind create cluster --name kubecause
kubectl get nodes
kubectl create namespace demo
```

然后启动服务：

```bash
KUBE_MODE=kind MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server
```

找不到 kubeconfig 时启动会明确报错，请确认已执行 `kind create cluster`，且 `~/.kube/config` 存在，或用 `KUBECONFIG` 指定。

### 环境变量一览

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `KUBE_MODE` | `fake` | `fake` 用 client-go fake clientset，预置 demo-api 数据；`kind` 用真 client-go 读 kubeconfig |
| `MODEL_MODE` | `stub` | `stub` 用桩模型，确定性走完工具调用；`real` 在 Slice 1 未实现，会明确报错 |
| `DB_PATH` | `./kubecause.db` | SQLite 文件路径 |
| `ADDR` | `:8080` | 监听地址 |
| `KUBECONFIG` | 空 | kind 模式下指定 kubeconfig 路径，不填用默认 |
| `MODEL_API_KEY` | 空 | Slice 2 真模型用，Slice 1 不用 |

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

# 4. 审批提案（Slice 1 只记录决定，不执行修复）
#    先从上一步的 proposal.id 里拿到提案 ID，例如 chg_xxxxxxxxxxxxxxxx
curl -s -X POST "http://localhost:8080/api/runs/$RUN_ID/approvals/$CHG_ID/decision" \
  -H 'Content-Type: application/json' \
  -d '{"decision":"approved","approver":"varo"}'
```

预期结果：任务状态从 `queued` 到 `running` 再到 `completed`；时间线里有 `list_workload_pods`、`get_pod`、`get_events`、`get_pod_logs`、`get_deployment`、`search_runbooks` 六步；诊断报告的原因为“容器内存限制过低，触发 OOMKilled 导致反复重启”，证据里包含 OOMKilled；提案为 `128Mi -> 256Mi`，状态 `pending`，批准后变成 `approved`，但 `executionStatus` 仍是 `not_started`。

## 目录说明

| 目录 | 职责 |
| --- | --- |
| `cmd/server/` | 入口：加载配置，组装模型、K8s 客户端和存储，启动 HTTP API 和 Worker |
| `cmd/eval/` | 离线评估：用固定样本跑桩模型 + fake 客户端，统计命中率和步数 |
| `internal/api/` | HTTP API：创建任务、查询快照、时间线轮询、审批决定 |
| `internal/run/` | Worker：领取任务、推进状态、重启恢复；工具适配器 |
| `internal/store/` | SQLite：迁移、任务、步骤、提案，步骤和进度同一事务提交 |
| `internal/agent/` | Agent 循环：步数上限、时限、工具校验、证据引用校验 |
| `internal/model/` | 模型适配器：stub 桩模型确定性走流程，real 在 Slice 1 明确报未实现 |
| `internal/kube/` | client-go 只读访问：fake 预置 demo-api，kind 读 kubeconfig |
| `internal/knowledge/` | 排障手册检索：内存关键词检索，覆盖 OOM、镜像拉取失败、探针失败 |
| `internal/repair/` | 修复占位：只生成提案，Slice 1 不执行，条件 Patch 的做法写在注释里 |
| `runbooks/` | 排障手册 Markdown |
| `testdata/cases/` | 评估用例，首批只有 OOM |
| `web/` | 极简静态演示页，由 Go 服务同源托管 |

## Slice 1 已实现

- 仓库骨架、SQLite 三张表（runs、run_steps、change_requests）、启动自动迁移
- 四个 HTTP 接口，契约按架构文档
- Worker 状态机 queued -> running -> completed/failed，单进程同时跑 API 和 Worker
- Agent 循环：默认 8 步、60 秒上限，工具白名单校验、namespace 一致性校验、证据引用校验
- 6 个工具：get_pod、list_workload_pods、get_events、get_pod_logs、get_deployment、search_runbooks，日志限 100 行 / 64KB
- fake 客户端预置 demo-api：Deployment、ReplicaSet、Pod（重启 5 次、上次 OOMKilled）、Events、日志
- 桩模型确定性走完 OOM 诊断，报告引用真实证据 ID
- 修复提案生成（OOM + Deployment + 高置信度时），审批幂等，重复提交返回当前结果
- 重启恢复：running 任务改回 queued，已提交步骤复用，不重复执行
- 离线评估入口和 OOM 用例

## Slice 1 未实现（Slice 2 / Slice 3 做）

- 真模型接入（`MODEL_MODE=real` 目前明确报错）
- SQLite FTS5 索引（目前是内存关键词检索，接口不变）
- 修复执行和 rollout 验证（审批只记录，`executionStatus` 保持 `not_started`）
- 完整 React + TypeScript 前端（目前是极简静态页）
- 多集群、多 namespace 演示、鉴权

## 架构文档

完整架构见 `ARCHITECTURE.md`（仓库外另存），架构图见 `docs/diagrams/architecture.png`。
