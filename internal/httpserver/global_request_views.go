package httpserver

import (
	"context"
	"strings"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
)

func (s *Server) globalRequestViews(ctx context.Context, events []cpamp.EventRow) []webui.GlobalRequestView {
	rows := make([]webui.GlobalRequestView, 0, len(events))
	ids, hashes := make([]string, 0), make([]string, 0, len(events))
	for _, event := range events {
		if event.PortalResourceID != 0 {
			if event.PortalUserID != "" {
				ids = append(ids, event.PortalUserID)
			}
		} else if hash := strings.ToLower(strings.TrimSpace(event.APIKeyHash)); hash != "" {
			hashes = append(hashes, hash)
		}
	}
	directory := store.EmptyUserDirectory()
	if s.Store != nil {
		var err error
		directory, err = s.Store.UserDirectoryFor(ctx, ids, hashes, false)
		if err != nil && s.Logger != nil {
			s.Logger.Error("load request owners", "error", err)
		}
	}
	for _, event := range events {
		row := webui.GlobalRequestView{RequestView: s.requestView(event), UserLabel: "外部 Key"}
		id := directory.Owners[strings.ToLower(strings.TrimSpace(event.APIKeyHash))]
		if event.PortalResourceID != 0 {
			row.UserLabel = emptyDash(event.PortalUserLabel)
			id = event.PortalUserID
		}
		if user, linked := directory.Users[id]; linked {
			row.User, row.UserLabel, row.Linked = s.userViewWithKey(user, directory.ActiveKeys[id]), user.Name, true
		}
		rows = append(rows, row)
	}
	return rows
}
