package domain

import "time"

type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

type UserStatus string

const (
	StatusPending          UserStatus = "pending"
	StatusRejected         UserStatus = "rejected"
	StatusApproved         UserStatus = "approved"
	StatusSuspended        UserStatus = "suspended"
	StatusSuspendedPending UserStatus = "suspended_pending"
	StatusDeletePending    UserStatus = "delete_pending"
	StatusDeleted          UserStatus = "deleted"
)

type User struct {
	ID                 string
	Phone              string
	Name               string
	PasswordHash       string
	Role               Role
	Status             UserStatus
	RejectionReason    string
	SuspensionReason   string
	PolicyVersion      int
	PolicyAcceptedAt   time.Time
	ApprovedAt         time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	LastLoginAt        time.Time
	DeletedAt          time.Time
	IssueCooldownUntil time.Time
}

func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

func (u User) CanUsePortalFeatures() bool {
	return u.Status == StatusApproved
}

type APIKey struct {
	ID         string
	UserID     string
	Hash       string
	LastFour   string
	Alias      string
	Status     string
	IssuedAt   time.Time
	RevokedAt  time.Time
	LastSeenAt time.Time
}

type ModelTestResult struct {
	UserID    string
	Model     string
	Status    string
	Message   string
	LatencyMS int64
	TestedAt  time.Time
}

type Policy struct {
	Version   int
	Title     string
	Body      string
	CreatedBy string
	CreatedAt time.Time
}

type AuditEvent struct {
	ID          int64
	ActorUserID string
	ActorLabel  string
	Action      string
	TargetID    string
	TargetLabel string
	Detail      string
	IP          string
	CreatedAt   time.Time
}

type SyncJob struct {
	ID        int64
	UserID    string
	Action    string
	KeyHash   string
	Attempts  int
	NextAt    time.Time
	LastError string
	CreatedAt time.Time
}
