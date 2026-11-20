# KubeCause kind 演示一键脚本（Mac 上跑）
# 前置条件：Docker Desktop 已启动，kind 集群已建好
#   kind create cluster --name kubecause
#   kubectl cluster-info --context kind-kubecause
# 用法：
#   bash scripts/kind/setup-demo.sh
#   # 从仓库根目录跑；脚本会自动找到它所在的目录
# 完成后手动验证（见脚本最后打印的命令）：
#   kubectl get pods -n demo
#   kubectl describe pod -n demo -l app=demo-api
#   kubectl logs -n demo -l app=demo-api --previous
set -euo pipefail
# 注意：全脚本的变量都必须写成 ${VAR} 的花括号形式，不能写 $VAR 后面直接跟中文标点。
# Mac 自带的 bash 是 3.2 版，$POD）这种写法会被它误读成一个不存在的变量名，
# 再加上 set -u 就会报 POD: unbound variable，直接退出。这就是之前第 33 行报错的原因。

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
YAML="${SCRIPT_DIR}/demo-api.yaml"

command -v kubectl >/dev/null 2>&1 || { echo "找不到 kubectl，请先 brew install kubectl" >&2; exit 1; }

echo "== 检查集群 =="
kubectl cluster-info --context kind-kubecause >/dev/null 2>&1 || kubectl cluster-info >/dev/null
kubectl get nodes

echo "== 应用 demo-api（namespace demo，内存 limit 128Mi，会真实 OOM）=="
kubectl apply -f "${YAML}"

echo "== 等待 Pod 出现 OOMKilled / 进入 CrashLoopBackOff（最多 180 秒）=="
# 直接等 Ready 会一直等不到，因为 Pod 会反复重启；
# 这里轮询检查 restartCount 和 OOMKilled，出现即算演示就绪。
for i in $(seq 1 60); do
  # 取第一个 Pod 的状态，失败时继续等
  POD="$(kubectl get pods -n demo -l app=demo-api -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [ -n "${POD}" ]; then
    echo "--- 第 ${i} 次检查（Pod: ${POD}）---"
    kubectl get pods -n demo -l app=demo-api
    # 用 describe 看 LastState 是否出现 OOMKilled
    if kubectl describe pod -n demo "${POD}" 2>/dev/null | grep -q "OOMKilled"; then
      echo "已观察到 OOMKilled，演示就绪。"
      break
    fi
    # 重启次数 >= 2 也算就绪（可能 describe 还没刷出 OOMKilled）
    RESTARTS="$(kubectl get pod -n demo "${POD}" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0)"
    if [ "${RESTARTS:-0}" -ge 2 ] 2>/dev/null; then
      echo "Pod 已重启 ${RESTARTS} 次，演示就绪。"
      break
    fi
  else
    echo "--- 第 ${i} 次检查：Pod 还没出现，继续等 ---"
  fi
  sleep 3
done

echo ""
echo "== 演示就绪，手动验证命令 =="
echo "kubectl get pods -n demo"
echo "kubectl describe pod -n demo -l app=demo-api"
echo "kubectl logs -n demo -l app=demo-api --previous"
echo ""
echo "== 然后启动 KubeCause（桩模型先跑通，再换真模型）=="
echo "KUBE_MODE=kind MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server"
echo "MODEL_API_KEY=\$MODEL_API_KEY KUBE_MODE=kind MODEL_MODE=real DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server"
echo ""
echo "清理演示：kubectl delete -f ${YAML}"
