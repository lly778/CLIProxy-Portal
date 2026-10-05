package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata"

	"cliproxy-portal/internal/backup"
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
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		return backupCommand(os.Args[2:])
	}
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

func backupCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("用法: portal backup configure|key|tick|restore")
	}
	fs := flag.NewFlagSet("backup "+args[0], flag.ContinueOnError)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	switch args[0] {
	case "configure":
		dir := fs.String("state-dir", "", "门户共享备份状态目录")
		repository := fs.String("repository", "", "专用私有仓库 owner/repo")
		tokenFile := fs.String("token-file", "", "令牌文件；不指定则沿用已有令牌")
		hour := fs.Int("hour", 3, "北京时间每周备份小时")
		minute := fs.Int("minute", 0, "北京时间每周备份分钟（0–59）")
		weekday := fs.Int("weekday", 1, "每周备份星期：0=周日，1=周一，...，6=周六")
		retain := fs.Int("retain", 3, "保留最近成功备份份数")
		enabled := fs.Bool("enabled", true, "启用每周定时备份")
		keySaved := fs.Bool("key-saved", false, "确认已将恢复密钥保存到服务器之外")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *dir == "" || *repository == "" || fs.NArg() != 0 {
			return errors.New("请指定 --state-dir 和 --repository")
		}
		c := backup.Config{Repository: *repository, Hour: *hour, Minute: *minute, Weekday: *weekday, Retain: *retain, Enabled: *enabled, KeySaved: *keySaved}
		if err := backup.Configure(ctx, *dir, c, *tokenFile); err != nil {
			return err
		}
		fmt.Println("备份后台配置已保存；未发起备份。")
		return nil
	case "key":
		dir := fs.String("state-dir", "", "门户共享备份状态目录")
		output := fs.String("output", "", "密钥导出路径；必须为新文件")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *dir == "" || *output == "" || fs.NArg() != 0 {
			return errors.New("请指定 --state-dir 和 --output")
		}
		if err := backup.ExportKey(*dir, *output); err != nil {
			return err
		}
		fmt.Println("恢复密钥已导出；请另行保管，不要仅留在原服务器。")
		return nil
	case "tick":
		dir := fs.String("state-dir", "", "门户共享备份状态目录")
		sources := fs.String("sources", "", "宿主机备份来源配置")
		restorePolicy := fs.String("restore-config", "", "宿主机数据库恢复策略；默认来源配置同目录 restore.json")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *dir == "" || *sources == "" || fs.NArg() != 0 {
			return errors.New("请指定 --state-dir 和 --sources")
		}
		return backup.Tick(ctx, *dir, *sources, *restorePolicy)
	case "restore":
		file := fs.String("file", "", "加密备份文件")
		key := fs.String("key-file", "", "单独保管的恢复密钥文件")
		output := fs.String("output", "", "全新的恢复目录，不覆盖现有文件")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *file == "" || *key == "" || *output == "" || fs.NArg() != 0 {
			return errors.New("请指定 --file、--key-file 和 --output")
		}
		if err := backup.Restore(ctx, *file, *key, *output); err != nil {
			return err
		}
		fmt.Println("备份校验通过，已恢复到新目录；尚未启动任何服务。")
		return nil
	default:
		return errors.New("未知备份命令")
	}
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
	var captureGateway *gateway.Gateway
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
		captureGateway = proxy
		app.CaptureVault = vault
		gatewayServer = newGatewayServer(cfg.GatewayListenAddr, proxy.Handler())
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
			listener, err := net.Listen("tcp", gatewayServer.Addr)
			if err != nil {
				errCh <- err
				return
			}
			errCh <- gatewayServer.Serve(gateway.TimingListener(listener))
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
			if err := captureGateway.WaitCaptures(shutdownCtx); err != nil {
				logger.Warn("gateway capture shutdown incomplete", "error", err)
			}
		}
		return server.Shutdown(shutdownCtx)
	}
}

// Model API connections follow the client and CPA lifetimes. Portal page
// deadlines must not impose an additional upload or generation deadline.
func newGatewayServer(addr string, handler http.Handler) *http.Server {
	server := &http.Server{Addr: addr, Handler: handler}
	gateway.ConfigureServerTiming(server)
	return server
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
