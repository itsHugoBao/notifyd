// notifyd：可靠的对外通知投递服务（见 specs/spec.md）。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"rc_hugobao/internal/api"
	"rc_hugobao/internal/config"
	"rc_hugobao/internal/dispatch"
	"rc_hugobao/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("notifyd 退出", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	st, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	handler := api.NewHandler(st, logger, cfg.MaxBodyBytes)
	srv := &http.Server{Addr: cfg.Addr, Handler: handler.Mux()}
	dispatcher := dispatch.New(st, cfg, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		dispatcher.Run(ctx) // ctx 取消后等在途投递完成才返回
	}()

	go func() {
		logger.Info("notifyd 启动", "addr", cfg.Addr, "db", cfg.DBPath, "workers", cfg.Workers)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server 退出", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("收到退出信号，优雅停机")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http 停机超时", "err", err)
	}
	wg.Wait() // 等 dispatcher 的在途投递收尾（受单次尝试超时约束）
	logger.Info("已退出")
	return nil
}
