# 技术设计与安全约束

本文解释 KubeCause 当前实现中与诊断、上下文、工具、审批和安全有关的关键机制。对应代码以最新 `main` 分支为准。

## 1. 两种工作模式

**对话模式**（`/api/conversations`）：模型可读取 Kubernetes 和 Runbook、调用已连接的 MCP 节点工具，输出文本、结构化卡片和可选的变更建议；**对话本身不直接写集群**。

**正式诊断任务**（`/api/runs`）：为任务分配 `runId`，持久化诊断步骤、报告与提案。Worker 异步推进任务，批准的提案交给 Repair 组件执行、读回和验证。

两种模式共用基础工具与模型适配逻辑，但运行状态和执行入口不同；由对话建议转来的正式任务可携带目标和具体变更建议，不必重新猜测资源目标。

## 2. Agent、上下文与知识检索

- **短期上下文**：每次对话默认取最近 10 条用户/助手消息；工具消息不占这 10 条名额。全量会话仍保存在 SQLite。
- **工具调用**：模型决定调用哪些只读工具，后端负责参数校验、执行和结果返回。一次模型响应包含多个 Tool Calls 时，当前实现**按返回顺序逐个执行**，不是并行执行。
- **工具结果**：对话把结构化工具结果映射到前端卡片和表格，并以 `round_id` 关联同一轮消息。
- **诊断报告**：正式任务由 Agent 组合证据并提交结构化报告，内置证据 ID 校验和步数/时间限制。
- **基础 RAG**：启动时从 `runbooks/*.md` 建立 SQLite FTS5 索引；检索失败时可回退关键词搜索。当前不是 Embedding 或向量检索。

自动摘要、向量长期记忆、混合检索等能力属于 [Roadmap](../ROADMAP.md)，不应与已有的短期消息上下文混淆。

## 3. MCP 节点诊断

真实集群模式可启动独立的 `cmd/mcp-server` 二进制，通过 MCP Client 的 initialize / `tools/list` 动态获取工具定义，并将工具追加到模型工具表。

模型调用 `node_*` 工具时，经 MCP `tools/call` 进入节点诊断执行器。当前工具为节点列表、磁盘、内存、进程和 kubelet 日志查询；该实现与将来规划的 **Kubernetes 工具 MCP Server** 是不同的工作。

参见 [MCP Client](mcp-client.md) 和 [Linux MCP Server](mcp-linux.md)。

## 4. 写操作的受控执行流程

```text
只读诊断 / 用户主动提出变更
    ↓
结构化修复建议或创建建议
    ↓
正式提案 → 人工审批
    ↓
执行前目标、字段和权限校验
    ↓
Dry-run / 当前版本核对
    ↓
Kubernetes 变更 → 读回 → Rollout 验证
```

建议卡不等于授权。默认只允许对 Deployment 的部分字段执行 Patch 或受约束的 Deployment 创建。

### Patch 示例

```json
{
  "patches": [
    {
      "op": "replace",
      "path": "/spec/template/spec/containers/0/image",
      "value": "nginx:1.25"
    }
  ]
}
```

仅允许 `replace`，当前白名单如下：

| 允许的路径 | 约束 |
| --- | --- |
| `/spec/replicas` | 副本数在受限范围内（默认 1–10） |
| `/spec/template/spec/containers/{i}/image` | 合法镜像地址 |
| `/spec/template/spec/containers/{i}/resources/requests/memory` | 内存请求值 |
| `/spec/template/spec/containers/{i}/resources/limits/memory` | 内存限制值 |
| `/spec/template/spec/containers/{i}/resources/requests/cpu` | CPU 请求值 |
| `/spec/template/spec/containers/{i}/resources/limits/cpu` | CPU 限制值 |

拒绝写入 `kube-system`、`kube-public`、`kube-node-lease` 等系统命名空间；不允许借助 Patch 修改 `hostNetwork`、`hostPID`、`privileged`、容器 `command` / `args`、资源名称或命名空间。

内存调整存在配置上限和增长比例限制（默认 `repair.maxMemoryMi=4096`、`repair.maxRatio=4`）。执行前做目标版本检查和 Dry-run，执行后读回、验证工作负载 Rollout。容器下标等信息以当前资源核对为准，而不是盲信模型给出的下标。

### 创建 Deployment

用户可从对话提出 Deployment 创建意图，模型补齐名称、命名空间、镜像等必要参数，再生成建议卡。创建同样受系统命名空间限制、镜像格式、1–10 副本和资源上限约束。

执行前检查同名资源是否已经存在；配置不一致时不可直接覆盖已有 Deployment。当前不提供直接创建裸 Pod 的写入口。

## 5. 存储与恢复

- `internal/store/`：SQLite 保存会话、消息、任务、步骤、报告和提案等状态。
- `internal/run/`：Worker 领取正式任务与批准的修复提案，处理任务状态。
- `internal/repair/`：执行前校验、条件变更、读回和 Rollout 验证。
- `internal/chat/`：上下文截取、模型对话、工具调用、建议卡及转任务逻辑。

进程重启时，会把中断状态的任务恢复为可领取状态，并尝试复用已经保存的步骤；写操作按目标状态和版本做幂等检查。恢复能力仍应在真实故障场景下验证，不能理解为任意步骤都具备分布式事务语义。

## 6. 安全边界与已知限制

- **当前未实现用户身份认证和审批权限控制**。请求字段里的审批人名称不是可靠身份凭证，不能把 API 直接暴露到公网且授予写权限。
- 读写客户端可使用独立 kubeconfig；生产化前需要配置最小权限 RBAC，不能仅依赖应用层白名单。
- Linux MCP 节点诊断依赖调试 Pod 和相应集群权限；其安全边界需单独评估。
- 真实模型的结论可能错误；Rollout 成功不能证明应用业务逻辑已恢复。
- 当前前端主要使用轮询来更新任务时间线，SSE 流式输出仍在规划中。

## 7. 主要代码位置

| 位置 | 职责 |
| --- | --- |
| `cmd/server/` | HTTP / Worker / MCP Client 装配入口 |
| `cmd/mcp-server/` | 独立 Linux MCP Server |
| `internal/api/` | 会话、任务、审批及配置 API |
| `internal/chat/` | 对话循环、短期上下文和建议卡 |
| `internal/agent/` | 正式诊断 Agent、工具校验与证据 |
| `internal/model/` | Stub 和 OpenAI 兼容模型 |
| `internal/mcpclient/` | MCP 工具发现与调用 |
| `internal/node/` | 节点诊断工具及执行器 |
| `internal/kube/` | Kubernetes 客户端 |
| `internal/knowledge/` | Runbook 检索 |
| `internal/store/` | SQLite 持久化 |
| `internal/run/` | 后台 Worker |
| `internal/repair/` | 受控写入与验证 |
| `frontend/` / `web/` | React 源码与内嵌构建资源 |

详细启动和测试步骤见 [安装与演示指南](getting-started.md)。
