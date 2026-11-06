package httpserver

import (
	"context"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/store"
)

func (s *Server) usageDirectory(ctx context.Context, stats []cpamp.APIKeyUsageStat, deltas ...[]domain.RequestUserDelta) (store.UserDirectory, error) {
	hashes := make([]string, 0, len(stats))
	for _, stat := range stats {
		hashes = append(hashes, stat.APIKeyHash)
	}
	users := []string{}
	for _, rows := range deltas {
		for _, d := range rows {
			if d.UserID != "" {
				users = append(users, d.UserID)
			}
		}
	}
	return s.Store.UserDirectoryFor(ctx, users, hashes, false)
}
