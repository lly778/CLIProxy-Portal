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
	"cliproxy-portal/internal/gateway"
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
	var gatewayServer *http.Server
	var captureVault *gateway.Vault
	if cfg.GatewayListenAddr != "" {
		vault, err := gateway.NewVault(cfg.GatewayCaptureDir, secret)
		if err != nil {
			return fmt.Errorf("initialize gateway capture storage: %w", err)
		}
		proxy, err := gateway.New(cfg.CPAUpstreamURL, keys, st, vault, logger)
		if err != nil {
			return fmt.Errorf("initialize CPA gateway: %w", err)
		}
		captureVault = vault
		app.CaptureVault = vault
		gatewayServer = &http.Server{Addr: cfg.GatewayListenAddr, Handler: proxy.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go background(ctx, st, keys, cfg, logger, captureVault)
	errCh := make(chan error, 1)
	go func() {
		logger.Info("portal listening", "address", cfg.ListenAddr, "external_url", cfg.ExternalURL)
		errCh <- server.ListenAndServe()
	}()
	if gatewayServer != nil {
		go func() {
			logger.Info("CPA gateway listening", "address", cfg.GatewayListenAddr)
			errCh <- gatewayServer.ListenAndServe()
		}()
	}
	select {
	case err := <-errCh:
		if gatewayServer != nil {
			_ = gatewayServer.Close()
		}
		_ = server.Close()
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		if gatewayServer != nil {
			_ = gatewayServer.Shutdown(shutdownCtx)
		}
		return server.Shutdown(shutdownCtx)
	}
}

func background(ctx context.Context, st *store.Store, keys *service.Keys, cfg config.Config, logger *slog.Logger, vault *gateway.Vault) {
	reconcile := time.NewTicker(cfg.ReconcileInterval)
	jobs := time.NewTicker(cfg.PendingRetry)
	cleanup := time.NewTicker(time.Hour)
	defer reconcile.Stop()
	defer jobs.Stop()
	defer cleanup.Stop()
	if err := keys.Reconcile(ctx); err != nil {
		logger.Warn("initial key reconciliation failed", "error", err)
	}
	cleanupGatewayCaptures(ctx, st, vault, logger)
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
			cleanupGatewayCaptures(ctx, st, vault, logger)
		}
	}
}

func cleanupGatewayCaptures(ctx context.Context, st *store.Store, vault *gateway.Vault, logger *slog.Logger) {
	if vault == nil {
		return
	}
	vault.LockSharedMessages()
	defer vault.UnlockSharedMessages()
	obsolete, err := st.DeleteGatewayCapturesExceptFormat(ctx, gateway.StructuredCaptureContentType)
	if err != nil {
		logger.Warn("obsolete gateway capture cleanup failed", "error", err)
		return
	}
	for _, id := range obsolete.CaptureIDs {
		vault.Delete(id)
	}
	for _, id := range obsolete.MessageIDs {
		vault.DeleteSharedMessage(id)
	}
	indexed, err := st.GatewayCaptureIDs(ctx)
	if err != nil {
		logger.Warn("gateway capture orphan scan failed", "error", err)
		return
	}
	if err := vault.PruneOrphans(indexed, time.Now().Add(-time.Hour)); err != nil {
		logger.Warn("gateway capture orphan cleanup failed", "error", err)
	}
	referenced, err := st.ReferencedGatewayMessageIDs(ctx)
	if err != nil {
		logger.Warn("gateway shared message orphan scan failed", "error", err)
		return
	}
	if err := vault.PruneSharedOrphans(referenced, time.Now().Add(-time.Hour)); err != nil {
		logger.Warn("gateway shared message orphan cleanup failed", "error", err)
	}
}
