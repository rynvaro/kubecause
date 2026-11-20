# KubeCause Slice 1
# 默认用 fake + stub，不需要真集群就能跑通全部测试和演示。
GO ?= go
export PATH := $(HOME)/workspace/tools/go/bin:$(PATH)

.PHONY: test vet build run-fake run-kind eval clean

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
run-kind:
	KUBE_MODE=kind MODEL_MODE=stub DB_PATH=./kubecause.db ADDR=:8080 $(GO) run ./cmd/server

eval:
	$(GO) run ./cmd/eval

clean:
	rm -f kubecause.db kubecause.db-wal kubecause.db-shm kubecause-server
