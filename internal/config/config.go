// Package config 从环境变量加载服务配置，所有项均有默认值（spec §7）。
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr              string        // NOTIFYD_ADDR 监听地址
	DBPath            string        // NOTIFYD_DB_PATH SQLite 文件路径
	Workers           int           // NOTIFYD_WORKERS 投递 worker 数
	PollInterval      time.Duration // NOTIFYD_POLL_INTERVAL dispatcher 轮询间隔
	AttemptTimeout    time.Duration // NOTIFYD_ATTEMPT_TIMEOUT 单次投递超时（spec §3.4）
	RetryBase         time.Duration // NOTIFYD_RETRY_BASE 退避基数（spec §5.3）
	RetryCap          time.Duration // NOTIFYD_RETRY_CAP 退避上限
	MaxAttempts       int           // NOTIFYD_MAX_ATTEMPTS 重试预算
	VisibilityTimeout time.Duration // NOTIFYD_VISIBILITY_TIMEOUT 崩溃恢复可见性超时（spec §5.5）
	MaxBodyBytes      int64         // NOTIFYD_MAX_BODY_BYTES 提交 body 上限（spec §3.1）
}

func Load() (Config, error) {
	cfg := Config{}
	var errs []error

	cfg.Addr = envStr("NOTIFYD_ADDR", ":8080")
	cfg.DBPath = envStr("NOTIFYD_DB_PATH", "notifyd.db")
	cfg.Workers = envInt("NOTIFYD_WORKERS", 8, &errs)
	cfg.PollInterval = envDur("NOTIFYD_POLL_INTERVAL", 500*time.Millisecond, &errs)
	cfg.AttemptTimeout = envDur("NOTIFYD_ATTEMPT_TIMEOUT", 10*time.Second, &errs)
	cfg.RetryBase = envDur("NOTIFYD_RETRY_BASE", 5*time.Second, &errs)
	cfg.RetryCap = envDur("NOTIFYD_RETRY_CAP", time.Hour, &errs)
	cfg.MaxAttempts = envInt("NOTIFYD_MAX_ATTEMPTS", 12, &errs)
	cfg.VisibilityTimeout = envDur("NOTIFYD_VISIBILITY_TIMEOUT", 60*time.Second, &errs)
	cfg.MaxBodyBytes = int64(envInt("NOTIFYD_MAX_BODY_BYTES", 256*1024, &errs))

	if len(errs) > 0 {
		return Config{}, errs[0]
	}
	if cfg.Workers < 1 || cfg.MaxAttempts < 1 {
		return Config{}, fmt.Errorf("workers 与 max attempts 必须 >= 1")
	}
	return cfg, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, errs *[]error) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

func envDur(key string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}
