package httpserver

import (
	"context"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
)

func (s *Server) usageDirectory(ctx context.Context, stats []cpamp.APIKeyUsageStat) (store.UserDirectory, error) {
	hashes := make([]string, 0, len(stats))
	for _, stat := range stats {
		hashes = append(hashes, stat.APIKeyHash)
	}
	return s.Store.UserDirectoryFor(ctx, nil, hashes, false)
}
