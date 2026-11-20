// Command server 是 KubeCause 的唯一入口：
// 单个 Go 进程同时跑 HTTP API 和后台 Worker。
//
// 切换开关（环境变量，不改代码就能切）：
//   KUBE_MODE=fake（默认）  用 client-go fake clientset，预置 demo-api 数据
//   KUBE_MODE=kind          用真 client-go 读 ~/.kube/config 或 KUBECONFIG
//   MODEL_MODE=stub（默认） 用桩模型，确定性走完工具调用
//   MODEL_MODE=real         Slice 2 真模型，Key 从 MODEL_API_KEY 读，
//                           缺失时启动报错；BaseURL 默认 DeepSeek，
//                           可用 MODEL_BASE_URL / MODEL_NAME 切换厂商
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
	"kubecause/internal/knowledge"
	"kubecause/internal/kube"
	"kubecause/internal/model"
	"kubecause/internal/repair"
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

	log.Printf("KubeCause Slice 3 启动：KUBE_MODE=%s MODEL_MODE=%s DB_PATH=%s ADDR=%s", kubeMode, modelMode, dbPath, addr)

	// 1. 打开 SQLite，启动时自动执行迁移。
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	// 2. 启动恢复：上次崩掉时停在 running 的任务改回 queued，步骤保留续跑；
	// 执行中途崩掉的提案改回待执行，重启后重新领取（重跑是安全的，
	// 执行前校验会先看当前值是否已是新值）。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if n, err := st.ResetRunningToQueued(ctx); err != nil {
		log.Fatalf("启动恢复失败: %v", err)
	} else if n > 0 {
		log.Printf("启动恢复：%d 个中断的任务已改回 queued，将从已提交步骤续跑", n)
	}
	if n, err := st.ResetExecutingChanges(ctx); err != nil {
		log.Fatalf("启动恢复（修复）失败: %v", err)
	} else if n > 0 {
		log.Printf("启动恢复：%d 个执行中的提案已改回待执行，将重新领取", n)
	}

	// 3. 按 KUBE_MODE 组装 K8s 客户端。读和写是两个独立的对象：
	// 读走 KubeClient（只读），写走 RepairClient（只 Patch 内存 limit）。
	// 本地 kind 演示为了简单仍用同一个 kubeconfig，但对象是分开建的；
	// 生产部署时把 REPAIR_KUBECONFIG 指向写身份专用的 kubeconfig，
	// 读身份和写身份就是两个不同的 ServiceAccount（见 scripts/kind/rbac.yaml）。
	var kc kube.KubeClient
	var rc kube.RepairClient
	switch kubeMode {
	case "fake":
		f := kube.NewFake()
		kc = f
		rc = f
		log.Printf("使用 fake K8s 客户端，预置 demo/demo-api 演示数据（读写接口分开，同一 fake 对象）")
	case "kind":
		real, err := kube.NewReal(os.Getenv("KUBECONFIG"))
		if err != nil {
			log.Fatalf("KUBE_MODE=kind 但无法连接集群: %v", err)
		}
		kc = real
		repairKubeconfig := os.Getenv("REPAIR_KUBECONFIG")
		if repairKubeconfig == "" {
			repairKubeconfig = os.Getenv("KUBECONFIG")
		}
		realRepair, err := kube.NewRealRepair(repairKubeconfig)
		if err != nil {
			log.Fatalf("无法创建写客户端: %v", err)
		}
		rc = realRepair
		log.Printf("使用真 K8s 客户端（kind），读取 kubeconfig；写客户端独立创建，生产上应换成写身份专用的 kubeconfig")
	default:
		log.Fatalf("KUBE_MODE 非法: %q，只能是 fake 或 kind", kubeMode)
	}

	// 4. 按 MODEL_MODE 组装模型。
	// Slice 2：real 从环境变量读 Key / BaseURL / ModelName，
	// Key 缺失时启动就明确报错，不让服务带着调不通的模型跑起来。
	var m agent.Model
	switch modelMode {
	case "stub":
		m = model.NewStub()
	case "real":
		real, err := model.NewRealFromEnv()
		if err != nil {
			log.Fatalf("MODEL_MODE=real 但无法创建真模型: %v", err)
		}
		m = real
		log.Printf("使用真模型：base_url=%s model=%s（Key 只从环境变量读，不写进文件）", env("MODEL_BASE_URL", "https://api.deepseek.com/v1"), env("MODEL_NAME", "deepseek-chat"))
	default:
		log.Fatalf("MODEL_MODE 非法: %q，只能是 stub 或 real", modelMode)
	}

	// 4b. 建 FTS5 索引：runbooks 目录按运行位置可能在不同层级，
	// 依次试几个候选，找不到就回退到关键词检索并记日志。
	// 索引建失败不让服务启动失败，检索仍可用，这是 Slice 2 的回退设计。
	kbIdx := knowledge.NewIndex(st.DB())
	built := false
	for _, dir := range []string{"runbooks", "../runbooks", "../../runbooks"} {
		if err := kbIdx.Build(ctx, dir); err == nil {
			log.Printf("FTS5 索引已建成，来源目录 %s", dir)
			built = true
			break
		}
	}
	if !built {
		log.Printf("FTS5 索引未建成（找不到 runbooks 目录），search_runbooks 将回退到关键词检索")
	}

	// 5. 组装工具、Worker、API。修复执行器用写客户端，Worker 循环里
	// 诊断没有任务时才会去领已批准的提案。
	tools := &run.ToolsAdapter{Kube: kc, KB: kbIdx}
	worker := run.NewWorker(st, tools, m)
	worker.SetRepair(repair.NewExecutor(st, rc))
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
