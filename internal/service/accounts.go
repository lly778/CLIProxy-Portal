package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/security"
	"cliproxy-portal/internal/store"
)

var (
	ErrInvalidCredentials = errors.New("手机号或密码错误")
	ErrAccountDeleted     = errors.New("账号不可用")
	ErrRegistrationClosed = errors.New("当前已暂停新注册")
)

type Accounts struct {
	Store     *store.Store
	Secret    []byte
	Now       func() time.Time
	UserIdle  time.Duration
	UserMax   time.Duration
	AdminIdle time.Duration
	AdminMax  time.Duration
}

func NewAccounts(st *store.Store, secret []byte) *Accounts {
	return &Accounts{Store: st, Secret: secret, Now: func() time.Time { return time.Now().UTC() }, UserIdle: 7 * 24 * time.Hour, UserMax: 30 * 24 * time.Hour, AdminIdle: 30 * time.Minute, AdminMax: 8 * time.Hour}
}

func (a *Accounts) Register(ctx context.Context, phoneInput, name, password string, policyVersion int, ip string) (domain.User, error) {
	open, err := a.Store.RegistrationOpen(ctx)
	if err != nil {
		return domain.User{}, err
	}
	if !open {
		return domain.User{}, ErrRegistrationClosed
	}
	phone, err := security.NormalizeMainlandPhone(phoneInput)
	if err != nil {
		return domain.User{}, err
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) < 2 || len([]rune(name)) > 50 {
		return domain.User{}, errors.New("姓名长度需要在 2 到 50 个字符之间")
	}
	if err := security.ValidatePassword(password, phone, false); err != nil {
		return domain.User{}, err
	}
	policy, err := a.Store.LatestPolicy(ctx)
	if err != nil {
		return domain.User{}, err
	}
	if policyVersion != policy.Version {
		return domain.User{}, errors.New("使用规则已更新，请重新确认")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return domain.User{}, err
	}
	now := a.Now()
	id, err := security.NewID("usr_")
	if err != nil {
		return domain.User{}, err
	}
	u := domain.User{ID: id, Phone: phone, Name: name, PasswordHash: hash, Role: domain.RoleUser, Status: domain.StatusPending, PolicyVersion: policy.Version, PolicyAcceptedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := a.Store.CreateUser(ctx, u); err != nil {
		if store.IsUniqueError(err) {
			return domain.User{}, errors.New("无法提交，请登录或联系管理员")
		}
		return domain.User{}, err
	}
	_ = a.audit(ctx, domain.AuditEvent{Action: "user.register", TargetID: u.ID, TargetLabel: maskedUser(u), IP: ip, CreatedAt: now})
	return u, nil
}

func (a *Accounts) Resubmit(ctx context.Context, user domain.User, name, password string, policyVersion int, ip string) error {
	if user.Status != domain.StatusRejected {
		return errors.New("当前状态不能重新提交")
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) < 2 || len([]rune(name)) > 50 {
		return errors.New("姓名长度需要在 2 到 50 个字符之间")
	}
	if err := security.ValidatePassword(password, user.Phone, false); err != nil {
		return err
	}
	policy, err := a.Store.LatestPolicy(ctx)
	if err != nil {
		return err
	}
	if policyVersion != policy.Version {
		return errors.New("使用规则已更新，请重新确认")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	if err := a.Store.ResubmitRejectedUser(ctx, user.ID, name, hash, policy.Version, a.Now()); err != nil {
		return err
	}
	return a.audit(ctx, domain.AuditEvent{ActorUserID: user.ID, ActorLabel: maskedUser(user), Action: "user.resubmit", TargetID: user.ID, TargetLabel: maskedUser(user), IP: ip, CreatedAt: a.Now()})
}

func (a *Accounts) Authenticate(ctx context.Context, phoneInput, password string) (domain.User, error) {
	phone, err := security.NormalizeMainlandPhone(phoneInput)
	if err != nil {
		return domain.User{}, ErrInvalidCredentials
	}
	u, err := a.Store.UserByPhone(ctx, phone)
	if err != nil {
		return domain.User{}, ErrInvalidCredentials
	}
	if u.Status == domain.StatusDeleted || u.PasswordHash == "" || !security.VerifyPassword(u.PasswordHash, password) {
		return domain.User{}, ErrInvalidCredentials
	}
	_ = a.Store.TouchLogin(ctx, u.ID)
	u.LastLoginAt = a.Now()
	return u, nil
}

func (a *Accounts) CreateSession(ctx context.Context, u domain.User, ip, ua string) (token string, csrf string, err error) {
	token, err = security.RandomToken(32)
	if err != nil {
		return "", "", err
	}
	now := a.Now()
	max := a.UserMax
	if u.IsAdmin() {
		max = a.AdminMax
	}
	v := store.Session{TokenHash: security.SHA256(token), UserID: u.ID, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(max), IP: ip, UserAgent: truncate(ua, 250)}
	if err = a.Store.CreateSession(ctx, v); err != nil {
		return "", "", err
	}
	return token, security.CSRFToken(a.Secret, token), nil
}

func (a *Accounts) ResolveSession(ctx context.Context, token string) (domain.User, store.Session, error) {
	if token == "" {
		return domain.User{}, store.Session{}, store.ErrNotFound
	}
	hash := security.SHA256(token)
	sess, err := a.Store.Session(ctx, hash)
	if err != nil {
		return domain.User{}, store.Session{}, err
	}
	now := a.Now()
	u, err := a.Store.UserByID(ctx, sess.UserID)
	if err != nil {
		return domain.User{}, store.Session{}, err
	}
	idle := a.UserIdle
	if u.IsAdmin() {
		idle = a.AdminIdle
	}
	if now.After(sess.ExpiresAt) || now.Sub(sess.LastSeen) > idle || u.Status == domain.StatusDeleted || u.Status == domain.StatusDeletePending {
		_ = a.Store.DeleteSession(ctx, hash)
		return domain.User{}, store.Session{}, store.ErrNotFound
	}
	if now.Sub(sess.LastSeen) > time.Minute {
		_ = a.Store.TouchSession(ctx, hash, now)
		sess.LastSeen = now
	}
	return u, sess, nil
}

func (a *Accounts) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return a.Store.DeleteSession(ctx, security.SHA256(token))
}

func (a *Accounts) ChangePassword(ctx context.Context, u domain.User, current, next string, currentSessionHash, ip string) error {
	if !security.VerifyPassword(u.PasswordHash, current) {
		return errors.New("当前密码错误")
	}
	if err := security.ValidatePassword(next, u.Phone, u.IsAdmin()); err != nil {
		return err
	}
	hash, err := security.HashPassword(next)
	if err != nil {
		return err
	}
	if err = a.Store.SetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	if err = a.Store.DeleteUserSessions(ctx, u.ID, currentSessionHash); err != nil {
		return err
	}
	return a.audit(ctx, domain.AuditEvent{ActorUserID: u.ID, ActorLabel: maskedUser(u), Action: "user.password.change", TargetID: u.ID, TargetLabel: maskedUser(u), IP: ip, CreatedAt: a.Now()})
}

func (a *Accounts) CreateAdmin(ctx context.Context, phoneInput, name, password string, actor *domain.User, ip string) (domain.User, error) {
	phone, err := security.NormalizeMainlandPhone(phoneInput)
	if err != nil {
		return domain.User{}, err
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) < 2 {
		return domain.User{}, errors.New("姓名至少需要 2 个字符")
	}
	if err := security.ValidatePassword(password, phone, true); err != nil {
		return domain.User{}, err
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return domain.User{}, err
	}
	id, err := security.NewID("usr_")
	if err != nil {
		return domain.User{}, err
	}
	now := a.Now()
	policy, _ := a.Store.LatestPolicy(ctx)
	u := domain.User{ID: id, Phone: phone, Name: name, PasswordHash: hash, Role: domain.RoleAdmin, Status: domain.StatusApproved, PolicyVersion: policy.Version, PolicyAcceptedAt: now, ApprovedAt: now, CreatedAt: now, UpdatedAt: now}
	if err = a.Store.CreateUser(ctx, u); err != nil {
		return domain.User{}, err
	}
	e := domain.AuditEvent{Action: "admin.create", TargetID: u.ID, TargetLabel: maskedUser(u), IP: ip, CreatedAt: now}
	if actor != nil {
		e.ActorUserID = actor.ID
		e.ActorLabel = maskedUser(*actor)
	}
	_ = a.audit(ctx, e)
	return u, nil
}

func (a *Accounts) GenerateReset(ctx context.Context, actor, target domain.User, ip string) (string, error) {
	code, err := security.NewResetCode()
	if err != nil {
		return "", err
	}
	now := a.Now()
	v := store.PasswordReset{CodeHash: security.HMAC(a.Secret, "reset:"+code), UserID: target.ID, ExpiresAt: now.Add(15 * time.Minute), CreatedAt: now}
	if err = a.Store.CreatePasswordReset(ctx, v); err != nil {
		return "", err
	}
	_ = a.audit(ctx, domain.AuditEvent{ActorUserID: actor.ID, ActorLabel: maskedUser(actor), Action: "user.password.reset_issued", TargetID: target.ID, TargetLabel: maskedUser(target), IP: ip, CreatedAt: now})
	return code, nil
}

func (a *Accounts) ResetPassword(ctx context.Context, phoneInput, code, password, ip string) error {
	phone, err := security.NormalizeMainlandPhone(phoneInput)
	if err != nil {
		return errors.New("重置信息无效或已过期")
	}
	u, err := a.Store.UserByPhone(ctx, phone)
	if err != nil {
		return errors.New("重置信息无效或已过期")
	}
	v, err := a.Store.PasswordReset(ctx, security.HMAC(a.Secret, "reset:"+strings.ToUpper(strings.TrimSpace(code))))
	if err != nil || v.UserID != u.ID || !v.UsedAt.IsZero() || a.Now().After(v.ExpiresAt) {
		return errors.New("重置信息无效或已过期")
	}
	if err = security.ValidatePassword(password, u.Phone, u.IsAdmin()); err != nil {
		return err
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	if err = a.Store.UsePasswordReset(ctx, v.CodeHash); err != nil {
		return errors.New("重置信息无效或已过期")
	}
	if err = a.Store.SetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	_ = a.Store.DeleteUserSessions(ctx, u.ID, "")
	return a.audit(ctx, domain.AuditEvent{Action: "user.password.reset", TargetID: u.ID, TargetLabel: maskedUser(u), IP: ip, CreatedAt: a.Now()})
}

func (a *Accounts) Approve(ctx context.Context, actor, target domain.User, note, ip string) error {
	if !actor.IsAdmin() {
		return errors.New("无权限")
	}
	if err := a.Store.ApproveUser(ctx, target.ID); err != nil {
		return err
	}
	return a.audit(ctx, event(actor, target, "user.approve", note, ip, a.Now()))
}
func (a *Accounts) Reject(ctx context.Context, actor, target domain.User, reason, ip string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("必须填写拒绝原因")
	}
	if err := a.Store.RejectUser(ctx, target.ID, reason); err != nil {
		return err
	}
	return a.audit(ctx, event(actor, target, "user.reject", reason, ip, a.Now()))
}
func (a *Accounts) ChangePhone(ctx context.Context, actor, target domain.User, phoneInput, ip string) error {
	if !actor.IsAdmin() {
		return errors.New("无权限")
	}
	phone, err := security.NormalizeMainlandPhone(phoneInput)
	if err != nil {
		return err
	}
	if err = a.Store.SetUserPhone(ctx, target.ID, phone); err != nil {
		return err
	}
	return a.audit(ctx, event(actor, target, "user.phone.change", fmt.Sprintf("%s -> %s", maskPhone(target.Phone), maskPhone(phone)), ip, a.Now()))
}

func event(actor, target domain.User, action, detail, ip string, at time.Time) domain.AuditEvent {
	return domain.AuditEvent{ActorUserID: actor.ID, ActorLabel: maskedUser(actor), Action: action, TargetID: target.ID, TargetLabel: maskedUser(target), Detail: truncate(detail, 500), IP: ip, CreatedAt: at}
}
func (a *Accounts) audit(ctx context.Context, e domain.AuditEvent) error {
	return a.Store.Audit(ctx, e)
}
func maskedUser(u domain.User) string {
	if u.Name == "" {
		return "匿名用户"
	}
	return u.Name + "(" + maskPhone(u.Phone) + ")"
}
func maskPhone(phone string) string {
	if len(phone) != 11 {
		return phone
	}
	return phone[:3] + "****" + phone[7:]
}
func truncate(value string, n int) string {
	value = strings.TrimSpace(value)
	if len(value) > n {
		return value[:n]
	}
	return value
}
