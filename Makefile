# KubeCause Slice 3
# 默认用 fake + stub，不需要真集群就能跑通全部测试和演示。
GO ?= go
export PATH := $(HOME)/workspace/tools/go/bin:$(PATH)

.PHONY: test vet build run-fake run-kind run-real eval eval-real clean

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

# fake 模式：假客户端 + 桩模型
run-fake:
	KUBE_MODE=fake MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 $(GO) run ./cmd/server

# kind 模式：真集群 + 桩模型（先在 Mac 上 kind create cluster）
# KUBE_MODE=kubeconfig 是通用模式，kind 是它的别名
run-kind:
	KUBE_MODE=kubeconfig MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 $(GO) run ./cmd/server

# k3s 模式：云服务器上的 k3s 也是读 kubeconfig，KUBECONFIG 指到 /etc/rancher/k3s/k3s.yaml
run-k3s:
	KUBE_MODE=kubeconfig MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 $(GO) run ./cmd/server

# kind + 真模型：MODEL_API_KEY 必须从环境变量传，不要写进 Makefile
run-real:
	KUBE_MODE=kubeconfig MODEL_MODE=real DB_PATH=./kubecause.db ADDR=:8080 $(GO) run ./cmd/server

eval:
	$(GO) run ./cmd/eval

# 真模型评估：无 Key 时明确跳过，不报错退出
eval-real:
	MODEL_MODE=real $(GO) run ./cmd/eval

clean:
	rm -f kubecause.db kubecause.db-wal kubecause.db-shm kubecause-server
