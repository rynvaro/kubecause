# 安装、配置与演示指南

本文覆盖从无集群的本地体验，到 kind 故障演示、真实模型、MCP 节点工具及开发测试的常用路径。请优先使用隔离的测试集群。

## 环境要求

- Go **1.25+**（以仓库 `go.mod` 为准）
- fake + stub：无需 Kubernetes 集群或模型 API Key
- kind 演示：Docker、kind、kubectl
- 修改前端：Node.js 和 npm

## 1. 本地体验（fake + stub）

```bash
git clone https://github.com/rynvaro/kubecause.git
cd kubecause
go mod download
make run
```

访问 http://localhost:8080，尝试：

```text
当前集群状态怎么样？
demo 中有哪些有问题的 Pod？
demo-api 为什么一直重启？
```

默认采用 client-go fake 客户端和 Stub 模型，用于验证功能与交互；不是对真实集群或真实 LLM 的评估。仓库包含前端构建产物；需要重新构建时运行 `make frontend`。

## 2. 使用 kind 重现故障

在 Docker 正常运行的 macOS 环境中：

```bash
brew install kind kubectl
kind create cluster --name kubecause
kubectl config current-context
kubectl get nodes

# OOMKilled 示例
bash scripts/kind/setup-demo.sh

# ImagePullBackOff 示例
bash scripts/kind/setup-bad-image.sh
```

启动 Go 服务：

```bash
KUBE_MODE=kubeconfig MODEL_MODE=stub go run ./cmd/server
```

检查示例：

```bash
kubectl get pods -n demo
kubectl describe pod -n demo -l app=demo-api
```

确认当前 kubeconfig 指向演示集群后，可清理：

```bash
kubectl delete -f scripts/kind/demo-api.yaml
kubectl delete -f scripts/kind/demo-bad-image.yaml
```

`KUBE_MODE=kubeconfig` 同样适用于 k3s、云厂商集群等能够使用 kubeconfig 的环境。`kind` 是兼容别名。

## 3. 使用真实模型

下面演示 DeepSeek 的 OpenAI 兼容接口：

```bash
export KUBECONFIG="$HOME/.kube/config"
export MODEL_API_KEY="你的模型 API Key"

KUBE_MODE=kubeconfig \
MODEL_MODE=real \
MODEL_BASE_URL=https://api.deepseek.com/v1 \
MODEL_NAME=deepseek-chat \
go run ./cmd/server
```

更换兼容的模型服务商时，需要按其接口调整地址、模型名和 API Key。不同服务商的 Tool Calling 支持情况可能有差异。

**重要：** `.env.example` 只是示例文件；服务端读取的是实际进程环境变量，**不会因为复制了 `.env` 文件就自动加载它**。不要把密钥写进代码或提交到 Git。

## 4. MCP 节点工具

MCP Server 是独立二进制，不由 `make build` 自动生成可执行文件名。先编译：

```bash
go build -o mcp-server ./cmd/mcp-server
```

在真实集群模式下，主服务默认从 `./mcp-server` 启动子进程并通过 stdio 发现工具；连接失败时会记录警告，其他诊断功能仍可使用。

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `MCP_SERVER_BIN` | `./mcp-server` | MCP Server 可执行文件 |
| `MCP_NODE_TOOLS` | `1` | 设为 `0` 可禁用节点工具 |

工具包括 `node_list`、`node_disk`、`node_memory`、`node_processes` 和 `node_logs`。这部分需要能创建节点调试 Pod 的集群权限，请阅读 [MCP Client 接入](mcp-client.md) 和 [Linux MCP Server](mcp-linux.md) 中的权限与限制说明。fake 模式不会连接真实节点工具。

## 5. 常用环境变量

| 变量 | 默认 | 用途 |
| --- | --- | --- |
| `KUBE_MODE` | `fake` | `fake` / `kubeconfig` |
| `MODEL_MODE` | `stub` | `stub` / `real` |
| `MODEL_API_KEY` | 空 | 真实模型认证 Key |
| `MODEL_BASE_URL` | `https://api.deepseek.com/v1` | 兼容模型接口地址 |
| `MODEL_NAME` | `deepseek-chat` | 模型名称 |
| `KUBECONFIG` | 默认 kubeconfig | 读取身份的配置 |
| `REPAIR_KUBECONFIG` | 复用读配置 | 写身份专用 kubeconfig |
| `DB_PATH` | `./kubecause.db` | SQLite 文件位置 |
| `ADDR` | `:8080` | HTTP 服务监听 |
| `CONFIG_PATH` | `config.yaml` | 运行参数配置 |
| `KUBE_DEBUG_MODEL` | 关闭 | `1` 表示记录模型请求日志 |

`config.yaml` 可管理对话步数、历史消息数、任务步数和超时、模型超时与温度、修复资源上限等参数。支持环境变量覆盖及设置页热更新；模型 API Key 不保存在配置中心。

## 6. REST API 使用示例

创建正式诊断任务：

```bash
curl -s -X POST http://localhost:8080/api/runs   -H 'Content-Type: application/json'   -d '{"namespace":"demo","kind":"Deployment","name":"demo-api","question":"demo-api 为什么一直重启"}'
```

将返回 JSON 中的 `runId` 保存为 `RUN_ID`，查询任务快照和时间线：

```bash
curl -s "http://localhost:8080/api/runs/$RUN_ID"
curl -s "http://localhost:8080/api/runs/$RUN_ID/events?after=0"
```

也可以通过 Web 页面提交诊断、查看报告，并对提案进行人工审批。审批结果会由后台 Worker 处理。**当前 API 不提供用户身份鉴权，请勿在具备写权限的情况下直接暴露到公网。**

## 7. 开发、测试与调试

```bash
make test       # Go 单元测试
make vet        # Go 静态检查
make build      # Go 构建检查
make eval       # Stub 离线评估
make frontend   # 构建 React 前端到 web/dist
```

前端开发模式：

```bash
cd frontend
npm install
npm run dev
```

Vite 默认使用 http://localhost:5173，并将 `/api` 请求代理到 Go 服务的 `:8080`。

模型请求调试：

```bash
KUBE_DEBUG_MODEL=1 go run ./cmd/server
```

调试输出可能含集群元数据、对话内容及模型响应。**仅在受控环境短时开启**，不要长期保留含敏感信息的完整日志。
