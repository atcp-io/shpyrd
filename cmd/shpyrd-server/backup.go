package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"shpyrd/pkg/backup"
	"shpyrd/pkg/install"
	"shpyrd/pkg/kube"
	"shpyrd/pkg/version"
)

// runBackup is `shpyrd-server backup` (RFC-0037): one export of the
// platform's state, encrypted with the passphrase in the environment and
// uploaded to the target, then pruning to the configured count. The
// CronJob of the platform-backup component runs it; `shpyrd cluster backup`
// runs it now as a Job.
func runBackup(logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	k, err := kube.Connect(kube.Options{})
	if err != nil {
		return err
	}
	target, err := backup.FromEnv()
	if err != nil {
		return err
	}
	st, err := openStore(logger, k, os.Getenv(install.VarDomain))
	if err != nil {
		return err
	}
	defer st.Close()
	passphrase := os.Getenv("SHPYRD_BACKUP_PASSPHRASE")
	if passphrase == "" {
		return fmt.Errorf("SHPYRD_BACKUP_PASSPHRASE is not set")
	}
	systemNS := envOr("SHPYRD_SYSTEM_NAMESPACE", install.DefaultSystemNamespace)
	cluster, domain, profile := os.Getenv(install.VarCluster), os.Getenv(install.VarDomain), os.Getenv(install.VarProfile)
	if info, err := install.ReadInstallInfo(ctx, k, systemNS); err == nil && info != nil {
		cluster = firstNonEmptyStr(info.Vars[install.VarCluster], cluster)
		domain = firstNonEmptyStr(info.Vars[install.VarDomain], domain)
		profile = firstNonEmptyStr(info.Profile, profile)
	}
	exp := &backup.Exporter{
		Kube: k.Kube, Dynamic: k.Dynamic, SystemNamespace: systemNS,
		SourceBase: envOr("SHPYRD_INTERNAL_URL", "http://shpyrd-server."+systemNS+".svc"),
		Domain:     domain, Cluster: firstNonEmptyStr(cluster, "shpyrd"), Profile: profile, Version: version.Version,
		Store: st,
	}

	var enc bytes.Buffer
	w, err := backup.Encrypt(&enc, passphrase)
	if err != nil {
		return err
	}
	man, err := exp.Export(ctx, w)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if err := w.Close(); err != nil {
		return err
	}
	name := exp.ArchiveName(man.CreatedAt)
	if err := target.Upload(ctx, name, bytes.NewReader(enc.Bytes()), int64(enc.Len())); err != nil {
		return err
	}
	logger.Info("backup uploaded", "name", name, "bytes", enc.Len(), "projects", len(man.Projects), "objects", man.Objects, "sources", man.Sources)
	if keep, _ := strconv.Atoi(os.Getenv("SHPYRD_BACKUP_KEEP")); keep > 0 {
		if n, err := target.Prune(ctx, keep); err != nil {
			logger.Warn("prune failed", "err", err.Error())
		} else if n > 0 {
			logger.Info("old backups removed", "count", n, "kept", keep)
		}
	}
	return nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

var _ io.Reader = (*bytes.Buffer)(nil)
