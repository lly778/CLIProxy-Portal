package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/httpserver"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("portal stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	st, err := store.Open(cfg.DatabasePath, cfg.RegistrationDefault)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()
	if len(os.Args) > 1 && os.Args[1] == "create-admin" {
		return createAdmin(st, os.Args[2:])
	}
	return serve(cfg, st)
}

func createAdmin(st *store.Store, args []string) error {
	fs := flag.NewFlagSet("create-admin", flag.ContinueOnError)
	phone := fs.String("phone", "", "中国大陆手机号")
	name := fs.String("name", "", "姓名")
	passwordFile := fs.String("password-file", "", "包含初始密码的文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *phone == "" || *name == "" || *passwordFile == "" {
		return errors.New("用法: portal create-admin --phone 13800138000 --name 管理员 --password-file /run/secrets/initial_admin_password")
	}
	password, err := config.ReadTextSecret(*passwordFile)
	if err != nil {
		return err
	}
	accounts := service.NewAccounts(st, []byte("create-admin-command-only-secret"))
	u, err := accounts.CreateAdmin(context.Background(), *phone, *name, password, nil, "cli")
	if err != nil {
		return err
	}
	fmt.Printf("管理员已创建：%s (%s)\n", u.Name, u.Phone)
	return nil
}

func serve(cfg config.Config, st *store.Store) error {
	secret, err := config.ReadSecret(cfg.AppSecretFile)
	if err != nil {
		return fmt.Errorf("read portal app secret: %w", err)
	}
	adminKey, err := config.ReadTextSecret(cfg.CPAMPAdminKeyFile)
	if err != nil {
		return fmt.Errorf("read CPAMP admin key: %w", err)
	}
	client, err := cpamp.NewWithConfig(cpamp.Config{BaseURL: cfg.CPAMPBaseURL, AdminKey: adminKey, Timeout: cfg.CPAMPTimeout})
	if err != nil {
		return err
	}
	accounts := service.NewAccounts(st, secret)
	keys := service.NewKeys(st, client)
	keys.CacheTTL = cfg.UsageCacheTTL
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	app, err := httpserver.New(cfg, st, accounts, keys, secret, logger)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: cfg.ListenAddr, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go background(ctx, st, keys, cfg, logger)
	errCh := make(chan error, 1)
	go func() {
		logger.Info("portal listening", "address", cfg.ListenAddr, "external_url", cfg.ExternalURL)
		errCh <- server.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		return server.Shutdown(shutdownCtx)
	}
}

func background(ctx context.Context, st *store.Store, keys *service.Keys, cfg config.Config, logger *slog.Logger) {
	reconcile := time.NewTicker(cfg.ReconcileInterval)
	jobs := time.NewTicker(cfg.PendingRetry)
	cleanup := time.NewTicker(time.Hour)
	defer reconcile.Stop()
	defer jobs.Stop()
	defer cleanup.Stop()
	if err := keys.Reconcile(ctx); err != nil {
		logger.Warn("initial key reconciliation failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcile.C:
			if err := keys.Reconcile(ctx); err != nil {
				logger.Warn("key reconciliation failed", "error", err)
			}
		case <-jobs.C:
			if err := keys.ProcessJobs(ctx, cfg.PendingRetry); err != nil {
				logger.Warn("pending key job failed", "error", err)
			}
		case <-cleanup.C:
			if err := st.CleanupSessions(ctx); err != nil {
				logger.Warn("session cleanup failed", "error", err)
			}
		}
	}
}
