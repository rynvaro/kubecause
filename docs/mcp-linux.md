# Linux 节点 MCP Server（mcp-linux）

独立进程，用官方 Go SDK（`github.com/modelcontextprotocol/go-sdk`）实现，
和 KubeCause 主程序（Kubernetes 故障诊断 Agent）分开。
只做一件事：让模型能查 Linux 节点的磁盘、内存、进程和 kubelet 日志。

## 5 个工具（全部只读）

| 工具 | 作用 | 参数 |
| --- | --- | --- |
| `node_list` | 列出集群的全部节点（节点名 + Ready 状态） | 无 |
| `node_disk` | 节点磁盘用量（`df -h`） | `node`（必填，节点名） |
| `node_memory` | 节点内存用量（`free -h`） | `node`（必填） |
| `node_processes` | 按内存占用排序的进程列表（`ps aux --sort=-%mem`），只返回前 20 行 | `node`（必填） |
| `node_logs` | kubelet 最近的日志（`journalctl -u kubelet --no-pager`） | `node`（必填）、`lines`（可选，默认 100，上限 500） |

`node_list` 是给模型拿节点名的：KubeCause 的 k8s 工具都不返回节点名，
没有它，模型连 `node_disk` 的 `node` 参数都填不出来。
查节点磁盘、内存或日志之前，先调 `node_list` 拿节点名。

每个工具只对应一条固定命令，绝不给任意 shell；
节点名按 Kubernetes 名字规则校验，带 shell 元字符的直接拒绝。

## 怎么启动

```bash
# 构建（Go ≥ 1.25，仓库 go.mod 已要求 1.25.0；见下）
go build -o mcp-server ./cmd/mcp-server

# 启动：走 stdio，KUBECONFIG 指向要查的集群
KUBECONFIG=~/.kube/config ./mcp-server
```

Go 版本注意：SDK v1.8.0 要求 Go ≥ 1.25。
VM 里 Go 1.23.12 靠 `GOTOOLCHAIN=auto` 自动下载了 1.25.0 工具链；
Mac 上也要装 Go 1.25 或更新（`brew install go` 后确认 `go version` ≥ 1.25），
或者同样靠 `GOTOOLCHAIN=auto` 自动下载。

## 怎么接 MCP inspector

```bash
# CLI 看工具列表（注意：inspector 默认不继承 shell 的环境变量，
# KUBECONFIG 必须用 -e 传进去）
npx @modelcontextprotocol/inspector --cli \
  -e KUBECONFIG=~/.kube/config \
  ./mcp-server --method tools/list

# CLI 调一个工具
npx @modelcontextprotocol/inspector --cli \
  -e KUBECONFIG=~/.kube/config \
  ./mcp-server --method tools/call \
  --tool-name node_disk --tool-arg node=kind-control-plane

# Web UI（浏览器里连、点着试）
npx @modelcontextprotocol/inspector
# 然后在 UI 里填 command = ./mcp-server 的绝对路径，
# Environment Variables 里加 KUBECONFIG=~/.kube/config
```

## 怎么接 Claude Desktop

`claude_desktop_config.json` 里加：

```json
{
  "mcpServers": {
    "kubecause-linux": {
      "command": "/绝对路径/mcp-server",
      "env": {
        "KUBECONFIG": "/Users/varo/.kube/config"
      }
    }
  }
}
```

## 调试 Pod 的机制

节点接入走 `kubectl debug` 的思路，只用 client-go，不调 kubectl 二进制，不走 SSH：

1. 给定节点名，先查节点存不存在，不存在直接报错（不用等超时）。
2. 在 `default` 命名空间建一个调试 Pod：
   `node-debugger-<节点名>-<6 位随机后缀>`，
   `spec.nodeName` 指向目标节点，`hostPID: true`，
   容器 `command: ["sleep", "infinity"]`，挂载节点根目录到 `/host`（`hostPath /`），
   `securityContext.privileged: true`。
3. 等 Pod 进入 Running（超时 90 秒），再用 client-go SPDY executor
   （exec 子资源）执行命令，收 stdout/stderr，每次 exec 超时 30 秒。
4. 用完就删 Pod（defer 清理，出错路径也删）。

环境变量：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `MCP_DEBUG_IMAGE` | `busybox:latest` | 调试 Pod 镜像。所有命令都走 `chroot /host`，用的是宿主机的二进制，镜像本身有没有 `free`/`journalctl` 不重要。`busybox sleep infinity` 是常见写法，经核对可以让容器持续运行。 |
| `MCP_DEBUG_POD_TIMEOUT_SECONDS` | `90` | 等 Pod 进入 Running 的超时秒数 |
| `MCP_DEBUG_EXEC_TIMEOUT_SECONDS` | `30` | 每次 exec 的超时秒数 |

## 已知局限

- **每次工具调用建删一次 Pod，比较慢**：建 Pod、等 Running、拉镜像都要时间，
  一次调用几秒到几十秒。v1 先跑通，Pod 复用以后再做。
- **没有列节点功能**：调用方要自己知道节点名（`kubectl get nodes`），
  server 没有顺带提供列节点工具。
- **k8s 的 11 个只读工具不在这个 server 里**：以后有时间再单独做 k8s 的 MCP。
- **没有鉴权**：stdio 本地进程，调用方就是本机用户；不要把它暴露到网络上。

## Mac 验证步骤（kind 集群）

```bash
git pull origin mcp-linux   # 或 git fetch origin && git checkout mcp-linux
go build -o mcp-server ./cmd/mcp-server
# KUBECONFIG 指到 kind（一般是 ~/.kube/config，current-context 是 kind-kubecause）
npx @modelcontextprotocol/inspector --cli \
  -e KUBECONFIG=$HOME/.kube/config \
  ./mcp-server --method tools/list
# 应该列出 5 个工具；先调 node_list 拿节点名，再逐个调一次（节点名换成 kind 的节点，一般是 kind-control-plane）：
npx @modelcontextprotocol/inspector --cli \
  -e KUBECONFIG=$HOME/.kube/config \
  ./mcp-server --method tools/call --tool-name node_list
npx @modelcontextprotocol/inspector --cli \
  -e KUBECONFIG=$HOME/.kube/config \
  ./mcp-server --method tools/call --tool-name node_disk --tool-arg node=kind-control-plane
npx @modelcontextprotocol/inspector --cli \
  -e KUBECONFIG=$HOME/.kube/config \
  ./mcp-server --method tools/call --tool-name node_memory --tool-arg node=kind-control-plane
npx @modelcontextprotocol/inspector --cli \
  -e KUBECONFIG=$HOME/.kube/config \
  ./mcp-server --method tools/call --tool-name node_processes --tool-arg node=kind-control-plane
npx @modelcontextprotocol/inspector --cli \
  -e KUBECONFIG=$HOME/.kube/config \
  ./mcp-server --method tools/call --tool-name node_logs --tool-arg node=kind-control-plane --tool-arg lines=20
# 每个调用后检查：kubectl get pods -n default -l app=kubecause-node-debugger
# 调试 Pod 应该已经建了又被删掉，不留残留。
```
