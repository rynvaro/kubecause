# Agent 接入 MCP Client（mcp-client）

主程序（KubeCause Agent）通过 MCP 协议调 Linux 节点工具：
主程序启动时把 `mcp-server` 当子进程拉起来，走 stdio 连上去，
`tools/list` 发现它的 5 个节点工具，并进模型的工具表；
模型调 `node_*` 时，分发走 MCP Client 的 `tools/call`，
结果像其他工具一样以 tool 消息回给模型。

Server 本身见 `docs/mcp-linux.md`。这里只写 Agent 侧怎么接。

## 怎么开

```bash
# 两个二进制都要编（Go ≥ 1.25）
go build -o kubecause-server ./cmd/server
go build -o mcp-server ./cmd/mcp-server

# 启动主程序：KUBECONFIG 指到 kind（一般是 ~/.kube/config）；
# MODEL_API_KEY 需已在 shell 里 export，这里不写出来（也不要写进文档和仓库）
KUBECONFIG=~/.kube/config KUBE_MODE=kubeconfig MODEL_MODE=real ./kubecause-server
```

启动日志里看到这一行就是接通了：

```
MCP 节点工具已接通（./mcp-server）：[node_disk node_list node_logs node_memory node_processes]
```

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `MODEL_API_KEY` | （无） | 真模型 Key。`MODEL_MODE=real` 时必须已在 shell 里 export；不要写进文档、命令和仓库，没设的话启动直接报错退出 |
| `MCP_SERVER_BIN` | `./mcp-server` | mcp-server 二进制路径（相对路径按主程序的工作目录算） |
| `MCP_NODE_TOOLS` | `1` | 设成 `0` 关掉节点工具，不拉 mcp-server 子进程 |

## 什么时候会尝试接

只在真集群模式（`KUBE_MODE=kubeconfig` 或 `kind`）下才尝试。
`KUBE_MODE=fake` 时不接：fake 模式没有真实集群，
节点工具就算接通也是查 `KUBECONFIG` 里的真集群，
和其他 fake 数据对不上，反而误导模型。

子进程继承主程序的环境变量，所以 `KUBECONFIG` 不用单独给
mcp-server 传——它读到的和主程序是同一个集群。

## 模型怎么看到节点工具

启动时 `initialize + tools/list` 拿到 5 个工具定义，
原样并进发给大模型的工具表：原来 12 个 + 节点 5 个 = 17 个。
模型自己决定调不调，后端不替模型挑。

节点工具和 k8s 工具走两条分发：

- `node_*`（`node_list` / `node_disk` / `node_memory` / `node_processes` / `node_logs`）
  → MCP Client → `tools/call` → 调试 Pod 里跑固定命令
- 其他 11 个 k8s 工具 → 原来的 ToolsAdapter → kube 客户端

校验也在两边各做一次：Agent 侧先校验节点名格式（DNS 子域名，
带 shell 元字符的直接拒绝）和 `node_logs` 的 `lines`（1-500），
Server 侧再校验一次。工具名只认启动时发现注册过的，
模型编一个没注册的名字会被拒绝。

## 节点名怎么来

KubeCause 的 k8s 工具都不返回节点名（`get_pod`、`list_pods`、
`get_cluster_overview` 都只有 Pod 和数量，没有 `spec.nodeName`），
模型本来连 `node_disk` 的 `node` 参数都填不出来。
所以 Server 上补了一个最小的 `node_list` 工具（只列节点名 + Ready 状态），
模型先调它拿节点名，再调另外 4 个。

## 接不上时怎么降级

以下情况都只记一条警告日志，节点工具不可用，其他诊断照常跑，
主流程绝不会因此挂掉：

- `MCP_SERVER_BIN` 指的二进制不存在（`fork/exec ...: no such file or directory`）
- 子进程拉起来但 `initialize` / `tools/list` 失败
- `MCP_NODE_TOOLS=0`（主动关掉，这时连警告都没有）

```
警告：MCP 节点工具不可用（连接 MCP Server 失败: ...），诊断继续，不影响其他工具；想关掉这个警告可以设 MCP_NODE_TOOLS=0
```

工具调用中途失败（例如集群连不上）时，Agent 记一步失败、
模型下一轮能看到失败原因、换个办法重试，和其他工具的失败处理一样。

## 两条线都接了

- 对话（`chat.Service`）：模型调 `node_*` 时走 MCP，
  结果以 tool 消息追加进历史，前端时间线能看到 `node_disk` 这一行。
- 单次诊断（`run.Worker` → `agent.Agent`）：同一套分发，
  节点工具的结果同样进证据，报告可以引用。

Stub 模型路径不受影响：`cmd/eval` 仍 3/3，
没有 MCP Client 时 stub 照常工作（stub 的剧本本来就不调节点工具）。

## 已知局限

- 第一次调节点工具比较慢：要先起调试 Pod、等它 Running
  （最多等 90 秒），之后每次调用也要建删一次 Pod。
  这是 Server 侧的设计（见 `docs/mcp-linux.md`），以后再做 Pod 复用。
- 节点工具只返回文本，没有结构化表格：前端时间线只显示摘要，
  详情在模型的回复文字里。
- 调试 Pod 的命名空间固定 `default`，用的是 kubeconfig 自己的身份。

## Mac 验证步骤（kind 集群）

```bash
git fetch origin && git checkout mcp-client
go build -o kubecause-server ./cmd/server
go build -o mcp-server ./cmd/mcp-server
# KUBECONFIG 指到 kind（current-context 是 kind-kubecause）；
# MODEL_API_KEY 需已在 shell 里 export，这里不写出来
KUBECONFIG=~/.kube/config KUBE_MODE=kubeconfig MODEL_MODE=real ./kubecause-server
# 启动日志里应有：MCP 节点工具已接通（./mcp-server）：[node_disk node_list ...]
```

然后在页面里问一个需要节点信息的问题，例如：

> demo-api 那个 Pod 一直 OOMKilled，它所在的节点内存怎么样？

期望看到：

1. 模型先调 `node_list` 拿节点名（一般是 `kind-control-plane`），
   再调 `node_memory`（或 `node_disk`）查那个节点；
   开 `KUBE_DEBUG_MODEL=1` 的话，`[debug model]` 日志里能看到
   `node_list` / `node_memory` 的工具调用。
2. 回复里有节点内存的真实数字，不只是 Pod 层面的 limit。
3. 每次节点工具调用后检查调试 Pod 没残留：
   `kubectl get pods -n default -l app=kubecause-node-debugger`
   应该为空。
4. 再试降级：停掉主程序，`MCP_SERVER_BIN=/nonexistent` 重启，
   日志里应有“节点工具不可用”的警告，页面对话仍能正常查 Pod。
