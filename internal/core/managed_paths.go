package core

import (
	"context"
	"errors"
	"strings"
)

type managedBackupKey struct{}

func withManagedBackup(ctx context.Context) context.Context {
	return context.WithValue(ctx, managedBackupKey{}, true)
}

func isManagedPath(key string) bool {
	for _, part := range strings.Split(key, "/") {
		if part == ".tamiops-backup" {
			return true
		}
	}
	return false
}

func checkManagedPath(ctx context.Context, key string) error {
	if allowed, _ := ctx.Value(managedBackupKey{}).(bool); !allowed && isManagedPath(key) {
		return errors.New("此目录由备份任务管理，请通过备份历史及保留策略操作")
	}
	return nil
}

func (s *Service) checkManagedResource(ctx context.Context, connectionID, key string) error {
	c, err := s.connection(connectionID)
	if err != nil {
		return err
	}
	_, absoluteKey := resource(c, key)
	return checkManagedPath(ctx, absoluteKey)
}

func (s *Service) checkManagedTree(ctx context.Context, connectionID string, tree TreePreview) error {
	if err := s.checkManagedResource(ctx, connectionID, tree.Path); err != nil {
		return err
	}
	for _, d := range tree.Directories {
		if err := s.checkManagedResource(ctx, connectionID, d); err != nil {
			return err
		}
	}
	for _, f := range tree.Files {
		if err := s.checkManagedResource(ctx, connectionID, f.Path); err != nil {
			return err
		}
	}
	return nil
}
