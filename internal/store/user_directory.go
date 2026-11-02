package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"cliproxy-portal/internal/domain"
)

func userDisplayFilter(status, query string) (string, []any) {
	where, args := "status<>'deleted'", []any{}
	if status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	if query = strings.TrimSpace(query); query != "" {
		where += " AND (phone LIKE ? OR name LIKE ?)"
		args = append(args, "%"+query+"%", "%"+query+"%")
	}
	return where, args
}

func (s *Store) CountUsersMatching(ctx context.Context, status, query string) (int, error) {
	where, args := userDisplayFilter(status, query)
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE "+where, args...).Scan(&count)
	return count, err
}

// ListUserDisplayPage is for sorts that do not depend on usage or key history.
// Read only the displayed page and display columns, without credential hashes.
func (s *Store) ListUserDisplayPage(ctx context.Context, status, query, order string, limit, offset int) ([]domain.User, error) {
	orders := map[string]string{"created_desc": "created_at_ms DESC,id", "created_asc": "created_at_ms,id", "name_asc": "lower(COALESCE(name,'')),id", "name_desc": "lower(COALESCE(name,'')) DESC,id"}
	column, ok := orders[order]
	if !ok {
		return nil, fmt.Errorf("unsupported user display order")
	}
	where, args := userDisplayFilter(status, query)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(phone,''),COALESCE(name,''),role,status,created_at_ms,last_login_at_ms FROM users WHERE `+where+" ORDER BY "+column+" LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []domain.User{}
	for rows.Next() {
		var user domain.User
		var created, login int64
		if err := rows.Scan(&user.ID, &user.Phone, &user.Name, &user.Role, &user.Status, &created, &login); err != nil {
			return nil, err
		}
		user.CreatedAt, user.LastLoginAt = fromMS(created), fromMS(login)
		users = append(users, user)
	}
	return users, rows.Err()
}

type UserDirectory struct {
	Users      map[string]domain.User
	Keys       map[string][]domain.APIKey
	Owners     map[string]string
	ActiveKeys map[string]domain.APIKey
}

func EmptyUserDirectory() UserDirectory {
	return UserDirectory{Users: make(map[string]domain.User), Keys: make(map[string][]domain.APIKey), Owners: make(map[string]string), ActiveKeys: make(map[string]domain.APIKey)}
}

// UserIdentitiesFor reads only display identities for the selected audit actors.
func (s *Store) UserIdentitiesFor(ctx context.Context, ids []string) (map[string]domain.User, error) {
	users := make(map[string]domain.User)
	if len(ids) == 0 {
		return users, nil
	}
	payload, _ := json.Marshal(ids)
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(name,''),COALESCE(phone,'') FROM users WHERE id IN (SELECT value FROM json_each(?))`, string(payload))
	if err != nil {
		return users, err
	}
	defer rows.Close()
	for rows.Next() {
		var user domain.User
		if err := rows.Scan(&user.ID, &user.Name, &user.Phone); err != nil {
			return users, err
		}
		users[user.ID] = user
	}
	return users, rows.Err()
}

// UserDirectoryFor batches only the selected users and key owners. No password
// hashes or unrelated user rows are loaded. Historical keys preserve attribution.
func (s *Store) UserDirectoryFor(ctx context.Context, userIDs, hashes []string, history bool) (UserDirectory, error) {
	result := EmptyUserDirectory()
	if len(userIDs) == 0 && len(hashes) == 0 {
		return result, nil
	}
	idsJSON, _ := json.Marshal(userIDs)
	normalized := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		if hash = strings.ToLower(strings.TrimSpace(hash)); hash != "" {
			normalized = append(normalized, hash)
		}
	}
	hashesJSON, _ := json.Marshal(normalized)
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,COALESCE(u.phone,''),COALESCE(u.name,''),u.role,u.status,u.created_at_ms,u.last_login_at_ms,
		COALESCE(k.id,''),COALESCE(k.user_id,''),COALESCE(k.key_hash,''),COALESCE(k.last_four,''),COALESCE(k.alias,''),COALESCE(k.status,''),COALESCE(k.issued_at_ms,0),COALESCE(k.revoked_at_ms,0),COALESCE(k.last_seen_at_ms,0)
		FROM users u LEFT JOIN api_keys k ON k.user_id=u.id AND (? OR k.status='active' OR k.key_hash IN (SELECT value FROM json_each(?)))
		WHERE u.id IN (SELECT value FROM json_each(?) UNION SELECT user_id FROM api_keys WHERE key_hash IN (SELECT value FROM json_each(?)))
		ORDER BY k.issued_at_ms,k.id`, history, string(hashesJSON), string(idsJSON), string(hashesJSON))
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var user domain.User
		var key domain.APIKey
		var created, login, issued, revoked, seen int64
		if err := rows.Scan(&user.ID, &user.Phone, &user.Name, &user.Role, &user.Status, &created, &login, &key.ID, &key.UserID, &key.Hash, &key.LastFour, &key.Alias, &key.Status, &issued, &revoked, &seen); err != nil {
			return result, err
		}
		user.CreatedAt, user.LastLoginAt = fromMS(created), fromMS(login)
		result.Users[user.ID] = user
		if key.ID == "" {
			continue
		}
		key.IssuedAt, key.RevokedAt, key.LastSeenAt = fromMS(issued), fromMS(revoked), fromMS(seen)
		result.Keys[user.ID] = append(result.Keys[user.ID], key)
		result.Owners[key.Hash] = user.ID
		if key.Status == "active" {
			result.ActiveKeys[user.ID] = key
		}
	}
	return result, rows.Err()
}
