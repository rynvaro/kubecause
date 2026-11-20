// Command server 是 KubeCause 的唯一入口：
// 单个 Go 进程同时跑 HTTP API 和后台 Worker。
//
// 切换开关（环境变量，不改代码就能切）：
//   KUBE_MODE=fake（默认）  用 client-go fake clientset，预置 demo-api 数据
//   KUBE_MODE=kind          用真 client-go 读 ~/.kube/config 或 KUBECONFIG
//   MODEL_MODE=stub（默认） 用桩模型，确定性走完工具调用
//   MODEL_MODE=real         Slice 1 未实现，会明确报错
//   DB_PATH                 SQLite 文件路径，默认 ./kubecause.db
//   ADDR                    监听地址，默认 :8080
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kubecause/internal/agent"
	"kubecause/internal/api"
	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/run"
	"kubecause/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := env("ADDR", ":8080")
	dbPath := env("DB_PATH", "./kubecause.db")
	kubeMode := strings.ToLower(env("KUBE_MODE", "fake"))
	modelMode := strings.ToLower(env("MODEL_MODE", "stub"))

	log.Printf("KubeCause Slice 1 启动：KUBE_MODE=%s MODEL_MODE=%s DB_PATH=%s ADDR=%s", kubeMode, modelMode, dbPath, addr)

	// 1. 打开 SQLite，启动时自动执行迁移。
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	// 2. 启动恢复：上次崩掉时停在 running 的任务改回 queued，步骤保留续跑。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if n, err := st.ResetRunningToQueued(ctx); err != nil {
		log.Fatalf("启动恢复失败: %v", err)
	} else if n > 0 {
		log.Printf("启动恢复：%d 个中断的任务已改回 queued，将从已提交步骤续跑", n)
	}

	// 3. 按 KUBE_MODE 组装 K8s 客户端。
	var kc kube.KubeClient
	switch kubeMode {
	case "fake":
		kc = kube.NewFake()
		log.Printf("使用 fake K8s 客户端，预置 demo/demo-api 演示数据")
	case "kind":
		real, err := kube.NewReal(os.Getenv("KUBECONFIG"))
		if err != nil {
			log.Fatalf("KUBE_MODE=kind 但无法连接集群: %v", err)
		}
		kc = real
		log.Printf("使用真 K8s 客户端（kind），读取 kubeconfig")
	default:
		log.Fatalf("KUBE_MODE 非法: %q，只能是 fake 或 kind", kubeMode)
	}

	// 4. 按 MODEL_MODE 组装模型。
	var m agent.Model
	switch modelMode {
	case "stub":
		m = model.NewStub()
	case "real":
		// Real.Next 会明确报错，不做假调用。
		m = model.NewReal(os.Getenv("MODEL_API_KEY"))
	default:
		log.Fatalf("MODEL_MODE 非法: %q，只能是 stub 或 real", modelMode)
	}

	// 5. 组装工具、Worker、API。
	tools := &run.ToolsAdapter{Kube: kc}
	worker := run.NewWorker(st, tools, m)
	go worker.RunLoop(ctx)

	handler := &api.Handler{Store: st}
	mux := http.NewServeMux()
	handler.Register(mux)
	// 极简静态演示页由 Go 服务同源托管，从仓库根目录运行时读 web/ 目录。
	// 刷新后用 runId 重新读任务。
	mux.Handle("/", http.FileServer(http.Dir("web")))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	fmt.Printf("KubeCause listening on %s (KUBE_MODE=%s MODEL_MODE=%s)\n", addr, kubeMode, modelMode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
}
