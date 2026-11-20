# KubeCause kind 新案例一键脚本：demo-bad-image 镜像拉取失败（Mac 上跑）
# 前置条件：Docker Desktop 已启动，kind 集群已建好
#   kind create cluster --name kubecause
#   kubectl cluster-info --context kind-kubecause
# 用法：
#   bash scripts/kind/setup-bad-image.sh
#   # 从仓库根目录跑；脚本会自动找到它所在的目录
# 完成后手动验证（见脚本最后打印的命令）：
#   kubectl get pods -n demo
#   kubectl describe pod -n demo -l app=demo-bad-image
# 然后在 KubeCause 对话里问“demo 那个一直拉不到镜像的是怎么回事”，
# 看 Agent 能不能自己发现镜像 tag 不存在，并给出换 tag 的建议，
# 而不是给一张内存修复卡（那是 bug，见 demo-bad-image.yaml 的注释）。
set -euo pipefail
# 注意：全脚本的变量都必须写成 ${VAR} 的花括号形式，不能写 $VAR 后面直接跟中文标点。
# Mac 自带的 bash 是 3.2 版，$POD）这种写法会被它误读成一个不存在的变量名，
# 再加上 set -u 就会报 POD: unbound variable，直接退出。这就是之前 setup-demo.sh
# 第 33 行报错的原因，这里同样要避开。

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
YAML="${SCRIPT_DIR}/demo-bad-image.yaml"

command -v kubectl >/dev/null 2>&1 || { echo "找不到 kubectl，请先 brew install kubectl" >&2; exit 1; }

echo "== 检查集群 =="
kubectl cluster-info --context kind-kubecause >/dev/null 2>&1 || kubectl cluster-info >/dev/null
kubectl get nodes

echo "== 应用 demo-bad-image（namespace demo，镜像 nginx:9.9.9-nonexistent 拉不到）=="
kubectl apply -f "${YAML}"

echo "== 等待 Pod 进入 ImagePullBackOff / ErrImagePull（最多 180 秒）=="
# 直接等 Ready 会一直等不到，因为镜像永远拉不到；
# 这里轮询检查 Pod 状态里是否出现 ImagePullBackOff 或 ErrImagePull，出现即算案例就绪。
for i in $(seq 1 60); do
  # 取第一个 Pod 的状态，失败时继续等
  POD="$(kubectl get pods -n demo -l app=demo-bad-image -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [ -n "${POD}" ]; then
    echo "--- 第 ${i} 次检查（Pod: ${POD}）---"
    kubectl get pods -n demo -l app=demo-bad-image
    # 用 describe 看事件里是否出现 Failed to pull image
    if kubectl describe pod -n demo "${POD}" 2>/dev/null | grep -q "Failed to pull image"; then
      echo "已观察到 Failed to pull image，案例就绪。"
      break
    fi
    # 状态里出现 ImagePullBackOff 或 ErrImagePull 也算就绪
    STATUS="$(kubectl get pod -n demo "${POD}" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)"
    if [ "${STATUS}" = "ImagePullBackOff" ] || [ "${STATUS}" = "ErrImagePull" ]; then
      echo "Pod 已进入 ${STATUS}，案例就绪。"
      break
    fi
  else
    echo "--- 第 ${i} 次检查：Pod 还没出现，继续等 ---"
  fi
  sleep 3
done

echo ""
echo "== 案例就绪，手动验证命令 =="
echo "kubectl get pods -n demo"
echo "kubectl describe pod -n demo -l app=demo-bad-image"
echo ""
echo "== 然后启动 KubeCause（kind + 真模型）=="
echo "KUBE_MODE=kubeconfig MODEL_MODE=real DB_PATH=./kubecause.db ADDR=:8080 go run ./cmd/server"
echo "MODEL_API_KEY=<redacted>"
echo ""
echo "== 在对话里问 Agent =="
echo "“demo 那个一直拉不到镜像的是怎么回事”"
echo "预期：Agent 查到 Failed to pull image，给出换 tag 的建议，不给内存修复卡。"
echo ""
echo "清理案例：kubectl delete -f ${YAML}"
