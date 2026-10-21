package httpserver

import (
	"context"
	"strings"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/webui"
)

func (s *Server) globalRequestViews(ctx context.Context, events []cpamp.EventRow) []webui.GlobalRequestView {
	rows := make([]webui.GlobalRequestView, 0, len(events))
	type owner struct {
		user   domain.User
		linked bool
	}
	owners := make(map[string]owner)
	for _, event := range events {
		hash := strings.ToLower(strings.TrimSpace(event.APIKeyHash))
		account, cached := owners[hash]
		if !cached && hash != "" {
			if key, keyErr := s.Store.KeyByHash(ctx, hash); keyErr == nil {
				if target, userErr := s.Store.UserByID(ctx, key.UserID); userErr == nil {
					account = owner{user: target, linked: true}
				}
			}
			owners[hash] = account
		}
		row := webui.GlobalRequestView{RequestView: s.requestView(event), UserLabel: "外部 Key"}
		if event.PortalResourceID != 0 {
			row.UserLabel = emptyDash(event.PortalUserLabel)
			if event.PortalUserID != "" {
				identity := "resource-user:" + event.PortalUserID
				account, cached := owners[identity]
				if !cached {
					if target, userErr := s.Store.UserByID(ctx, event.PortalUserID); userErr == nil {
						account = owner{user: target, linked: true}
					}
					owners[identity] = account
				}
				if account.linked {
					row.User, row.UserLabel, row.Linked = s.userView(account.user), account.user.Name, true
				}
			}
			rows = append(rows, row)
			continue
		}
		if account.linked {
			row.User, row.UserLabel, row.Linked = s.userView(account.user), account.user.Name, true
		}
		rows = append(rows, row)
	}
	return rows
}
