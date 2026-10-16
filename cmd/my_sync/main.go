// Command my_sync backs up directories into encrypted, size-split zip
// archives with an incremental index, and serves a management web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"my_sync/internal/backup"
	"my_sync/internal/config"
	"my_sync/internal/index"
	"my_sync/internal/service"
	"my_sync/internal/web"
)

var version = "dev"

const usage = `my_sync %s - 增量加密分卷备份

用法:
  my_sync [serve]   [-config FILE]                 启动管理界面与定时任务 (默认)
  my_sync backup    [-config FILE] [-dry-run]      执行一次备份
  my_sync list      [-config FILE]                 列出已有备份
  my_sync restore   [-config FILE] -to DIR [-id BACKUP_ID] [-prefix PATH]
                                                   按数据库恢复文件 (默认恢复到最新状态)
  my_sync version

配置文件默认取环境变量 MYSYNC_CONFIG，否则为 ./config.yaml
`

func main() {
	log.SetFlags(log.LstdFlags)
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "backup":
		err = cmdBackup(args)
	case "list":
		err = cmdList(args)
	case "restore":
		err = cmdRestore(args)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func defaultConfigPath() string {
	if p := os.Getenv("MYSYNC_CONFIG"); p != "" {
		return p
	}
	return "config.yaml"
}

func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	return fs, cfgPath
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cmdServe(args []string) error {
	fs, cfgPath := newFlags("serve")
	fs.Parse(args)
	store, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	cfg := store.Get()
	svc := service.New(store)
	ctx, stop := signalContext()
	defer stop()
	svc.Start(ctx)

	handler, err := web.New(svc)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("my_sync %s 管理界面监听 %s, 配置文件 %s", version, cfg.Listen, store.Path())
	if _, _, enabled := config.UICredentials(); !enabled {
		log.Printf("警告: 未设置管理界面登录账号 (环境变量 MYSYNC_UI_USER / MYSYNC_UI_PASSWORD)")
	}
	warnLegacyPasswordEnv()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Give a running backup the chance to observe cancellation and clean up.
	for i := 0; i < 50 && svc.Running(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// warnLegacyPasswordEnv flags MYSYNC_PASSWORD, which older versions used to
// override the archive password and which is now ignored.
func warnLegacyPasswordEnv() {
	if _, ok := config.EnvSet(config.EnvLegacyPassword); ok {
		log.Printf("警告: 环境变量 %s 已不再生效，压缩包密码以配置文件 / 管理页面为准", config.EnvLegacyPassword)
	}
}

type cliReporter struct{ last time.Time }

func (r *cliReporter) Log(msg string) { log.Println(msg) }

func (r *cliReporter) Progress(p backup.Progress) {
	if p.Phase != backup.PhaseArchiving || time.Since(r.last) < 5*time.Second {
		return
	}
	r.last = time.Now()
	log.Printf("进度: %d/%d 个文件, %s/%s", p.FilesDone, p.FilesTotal,
		config.FormatSize(p.BytesDone), config.FormatSize(p.BytesTotal))
}

func cmdBackup(args []string) error {
	fs, cfgPath := newFlags("backup")
	dry := fs.Bool("dry-run", false, "只列出增量内容，不写入任何文件")
	fs.Parse(args)
	store, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	warnLegacyPasswordEnv()
	ctx, stop := signalContext()
	defer stop()
	res, err := backup.Run(ctx, store.Get(), backup.Options{DryRun: *dry}, &cliReporter{})
	if err != nil {
		return err
	}
	if *dry {
		for _, c := range res.Changes {
			fmt.Printf("%-8s %-8s %s\n", c.Op, c.Skip, c.Path)
		}
		if n := res.Added + res.Modified + res.Deleted + res.Deferred + res.Oversize; n > len(res.Changes) {
			fmt.Printf("... 共 %d 条\n", n)
		}
	}
	return nil
}

func cmdList(args []string) error {
	fs, cfgPath := newFlags("list")
	fs.Parse(args)
	store, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	svc := service.New(store)
	backups, err := svc.Backups()
	if err != nil {
		return err
	}
	fmt.Printf("%-20s %-20s %6s %6s %6s %12s %4s %8s\n", "ID", "完成时间", "新增", "修改", "删除", "归档大小", "卷", "跟踪文件")
	for _, b := range backups {
		fmt.Printf("%-20s %-20s %6d %6d %6d %12s %4d %8d\n", b.BackupID, b.FinishedAt.Local().Format("2006-01-02 15:04:05"),
			b.Stats.Added, b.Stats.Modified, b.Stats.Deleted, config.FormatSize(b.Stats.ArchiveBytes), len(b.Volumes), b.Stats.TotalFiles)
	}
	return nil
}

func cmdRestore(args []string) error {
	fs, cfgPath := newFlags("restore")
	to := fs.String("to", "", "恢复到的目录 (必填)")
	id := fs.String("id", "", "要恢复的备份 ID，默认最新")
	prefix := fs.String("prefix", "", "只恢复以此开头的路径，如 docs/ 或 docs/report.xlsx")
	fs.Parse(args)
	if *to == "" {
		return errors.New("必须指定 -to")
	}
	if *id != "" && !index.IsBackupID(*id) {
		return fmt.Errorf("非法备份 ID %q", *id)
	}
	store, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	warnLegacyPasswordEnv()
	cfg := store.Get()
	ctx, stop := signalContext()
	defer stop()
	res, err := backup.Restore(ctx, backup.RestoreOptions{
		Dest: cfg.Dest, BackupID: *id, Target: *to, Prefix: *prefix, Password: cfg.Password,
	}, func(s string) { log.Println(s) })
	if res != nil {
		log.Printf("已从备份 %s 恢复 %d 个文件 (%s) 到 %s", res.BackupID, res.Files, config.FormatSize(res.Bytes), *to)
	}
	return err
}
