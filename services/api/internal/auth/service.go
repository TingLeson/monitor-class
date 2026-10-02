package auth

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/classwatch/classwatch/services/api/internal/infrastructure/logging"
	"github.com/classwatch/classwatch/services/api/internal/user"
)

// Service-level sentinel errors.
//
// They are the only vocabulary the HTTP layer branches on, and they are
// deliberately coarse:
//
//   - ErrInvalidCredentials covers "no such account" AND "wrong password" (§58).
//     A caller cannot accidentally tell them apart, because the service does not.
//   - ErrAccountDisabled is separate because the product needs it: the frontend
//     must say "this account is disabled" instead of looping on a login form.
//   - ErrSessionInvalid covers missing, unknown, expired and revoked sessions.
var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrAccountDisabled    = errors.New("auth: account is disabled")
	ErrSessionInvalid     = errors.New("auth: session is invalid")
)

// maxUserAgentLength bounds the stored User-Agent. It is diagnostic data, so a
// truncated header is still useful, while an unbounded one is a free way to bloat
// the sessions table from the outside.
const maxUserAgentLength = 512

// Config is the service's runtime policy.
type Config struct {
	// SessionTTL is the absolute session lifetime. Fixed, never extended.
	SessionTTL time.Duration
	// IdleTouchInterval throttles writes to sessions.last_seen_at.
	IdleTouchInterval time.Duration
	// PasswordPolicy is applied to new passwords (adminctl, password reset).
	PasswordPolicy PasswordPolicy
}

// LoginMeta is the request-derived context stored with a session.
//
// It is deliberately small: a User-Agent and an IP are what an operator needs to
// answer "where did this session come from?" during an incident, and neither is
// used for authorization.
type LoginMeta struct {
	UserAgent string
	IP        *netip.Addr
}

// Principal is the authenticated caller of one request.
//
// It is built from the database join (sessions ⋈ users) on every request and
// never from the cookie, so a client cannot influence its own role or status. The
// extra account fields (Status, CreatedAt, LastLoginAt) are carried because the
// /auth/me response needs them and re-querying the same row would be waste — they
// are still current values read in the same statement, not a stale snapshot.
type Principal struct {
	UserID      uuid.UUID
	Account     string
	DisplayName string
	Role        user.Role
	Status      user.Status
	CreatedAt   time.Time
	LastLoginAt *time.Time

	SessionID uuid.UUID
	// CSRFToken is compared against X-CSRF-Token on every unsafe request.
	CSRFToken string
}

// LoginResult is what a successful login hands back to the HTTP layer.
type LoginResult struct {
	// Principal is the freshly authenticated caller.
	Principal Principal
	// User is the full account row, for building the response DTO. It contains
	// PasswordHash; serialising this struct directly is what the HTTP layer must
	// never do (it builds its own DTO instead).
	User *user.User
	// SessionToken is the RAW token. It is returned exactly once, to be set as a
	// cookie, and is never logged or persisted server-side.
	SessionToken string
	// CSRFToken mirrors Principal.CSRFToken, for the second cookie.
	CSRFToken string
	ExpiresAt time.Time
}

// Service implements login, session authentication and logout.
type Service struct {
	users    user.Repository
	sessions SessionStore
	cfg      Config
	// now is injectable so the session lifecycle (expiry, idle touch) can be
	// tested without sleeping.
	now func() time.Time
}

// NewService wires the service. A zero SessionTTL is a programming error and is
// replaced by a conservative default rather than producing sessions that expire
// immediately.
func NewService(users user.Repository, sessions SessionStore, cfg Config) *Service {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 24 * time.Hour
	}
	if cfg.PasswordPolicy.MinLength < MinPasswordLengthFloor {
		cfg.PasswordPolicy = NewPasswordPolicy(cfg.PasswordPolicy.MinLength)
	}
	return &Service{users: users, sessions: sessions, cfg: cfg, now: time.Now}
}

// LoginStudent authenticates the account-only student entry (§2.2/§38).
//
// There is no password by design: a student account is a name the teacher wrote
// on a list, and pretending otherwise would invent an authentication factor the
// product does not have. What the system DOES owe here is everything else on the
// §2.2 list — status check, session management, rate limiting (enforced by the
// HTTP middleware) and server-side authorization — and an unknown account must
// be indistinguishable from a wrong one so the login form cannot be used to
// enumerate who is in which class.
func (s *Service) LoginStudent(ctx context.Context, account string, meta LoginMeta) (*LoginResult, error) {
	u, err := s.findByAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	// A teacher or admin account typed into the student entry is NOT "not a
	// student, please use the other URL": that would confirm the account exists
	// and name its role. It is simply invalid credentials.
	if u.Role != user.RoleStudent {
		logLoginRejected(ctx, user.RoleStudent, u, "role_mismatch")
		return nil, ErrInvalidCredentials
	}
	if u.Status != user.StatusActive {
		logLoginRejected(ctx, user.RoleStudent, u, "account_disabled")
		return nil, ErrAccountDisabled
	}
	return s.issueSession(ctx, u, meta)
}

// LoginWithPassword authenticates the teacher and admin entries (§39/§40).
//
// Order matters twice:
//
//  1. The password is verified BEFORE the status is inspected. Reporting
//     "disabled" to anyone who merely knows an account name would turn the login
//     form into an existence oracle; requiring the correct password first means
//     only someone who could have logged in anyway learns that the account is
//     switched off.
//  2. An unknown account still pays for one Argon2id verification (see
//     verifyDummyPassword). Returning immediately would make "no such account"
//     measurably faster than "wrong password", which is the same oracle by a
//     different channel.
func (s *Service) LoginWithPassword(ctx context.Context, role user.Role, account, password string, meta LoginMeta) (*LoginResult, error) {
	if role != user.RoleTeacher && role != user.RoleAdmin {
		// A programming error: this method serves the two password entries only.
		return nil, errors.New("auth: LoginWithPassword called for a role without passwords")
	}

	u, err := s.users.FindByAccount(ctx, account)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			verifyDummyPassword(password)
			logLoginRejected(ctx, role, nil, "unknown_account")
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	// Wrong entry point (a teacher account on the admin form, or a student
	// account) and wrong password produce the same answer.
	if u.Role != role || !u.HasPassword() {
		verifyDummyPassword(password)
		logLoginRejected(ctx, role, u, "role_mismatch")
		return nil, ErrInvalidCredentials
	}

	ok, needsRehash, err := Verify(*u.PasswordHash, password)
	if err != nil {
		// The stored hash is unparseable: every login for this account is broken.
		// That is an operator problem, not a credential problem, so it must not be
		// dressed up as a wrong password.
		return nil, err
	}
	if !ok {
		logLoginRejected(ctx, role, u, "wrong_password")
		return nil, ErrInvalidCredentials
	}

	if u.Status != user.StatusActive {
		logLoginRejected(ctx, role, u, "account_disabled")
		return nil, ErrAccountDisabled
	}

	if needsRehash {
		// The password is correct and in hand, which is the only moment a
		// transparent upgrade is possible: the plaintext is never stored, so a
		// hash can never be re-derived later. A failure here must not fail the
		// login — the user is authenticated either way, and the old parameters
		// still verify (just more weakly than the current ones).
		s.rehash(ctx, u, password)
	}

	return s.issueSession(ctx, u, meta)
}

// Authenticate resolves a raw session token into a Principal.
//
// Every rejection returns ErrSessionInvalid except a disabled account, which is
// reported as such because the product distinguishes it in the UI. A disabled
// account's session is revoked on the spot: otherwise re-enabling the account
// would resurrect a session that was created before it was switched off, and
// "disabled" would have been a pause rather than a stop (§37).
func (s *Service) Authenticate(ctx context.Context, rawToken string) (*Principal, error) {
	if strings.TrimSpace(rawToken) == "" {
		return nil, ErrSessionInvalid
	}

	sw, err := s.sessions.FindByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return nil, ErrSessionInvalid
		}
		return nil, err
	}

	// Defence in depth: the store already filters revoked and expired rows, but
	// it is an interface, and a future implementation (a Redis cache, a replica
	// read) may not. The policy lives here so it cannot be optimised away.
	now := s.now()
	if sw.RevokedAt != nil || !now.Before(sw.ExpiresAt) {
		return nil, ErrSessionInvalid
	}

	if sw.UserStatus != user.StatusActive {
		if err := s.sessions.Revoke(ctx, sw.ID); err != nil {
			// Not fatal for this request: the session is already unusable because
			// the account is disabled. It is logged because a failing revoke means
			// the row will still look live in an audit.
			logging.FromContext(ctx).Warn("failed to revoke session of disabled account",
				"error", err, "session_id", sw.ID.String(), "user_id", sw.UserID.String())
		}
		logging.FromContext(ctx).Info("session rejected: account disabled",
			logging.FieldUserID, sw.UserID.String(),
			logging.FieldRole, string(sw.Role),
			logging.FieldSessionID, sw.ID.String(),
		)
		return nil, ErrAccountDisabled
	}

	s.touchIfIdle(ctx, sw, now)

	return &Principal{
		UserID:      sw.UserID,
		Account:     sw.Account,
		DisplayName: sw.DisplayName,
		Role:        sw.Role,
		Status:      sw.UserStatus,
		CreatedAt:   sw.UserCreatedAt,
		LastLoginAt: sw.UserLastLoginAt,
		SessionID:   sw.ID,
		CSRFToken:   sw.CSRFToken,
	}, nil
}

// Logout revokes the session behind a raw token.
//
// It is idempotent by contract: an unknown, expired or already revoked token is a
// success, because the caller's goal ("this browser must no longer be logged in")
// is already true. Answering 401 here would make a double-click on "log out" look
// like a failure and would tell an attacker which tokens exist.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if strings.TrimSpace(rawToken) == "" {
		return nil
	}
	sw, err := s.sessions.FindByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return nil
		}
		return err
	}
	if err := s.sessions.Revoke(ctx, sw.ID); err != nil {
		return err
	}
	logging.FromContext(ctx).Info("session revoked",
		logging.FieldUserID, sw.UserID.String(),
		logging.FieldRole, string(sw.Role),
		logging.FieldSessionID, sw.ID.String(),
		"reason", "logout",
	)
	return nil
}

// RevokeSession revokes one session by id (used by "disable account" flows in
// later phases and by adminctl).
func (s *Service) RevokeSession(ctx context.Context, id uuid.UUID) error {
	return s.sessions.Revoke(ctx, id)
}

// findByAccount normalises and looks up an account.
func (s *Service) findByAccount(ctx context.Context, account string) (*user.User, error) {
	trimmed := strings.TrimSpace(account)
	if trimmed == "" {
		return nil, ErrInvalidCredentials
	}
	u, err := s.users.FindByAccount(ctx, trimmed)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			logLoginRejected(ctx, user.RoleStudent, nil, "unknown_account")
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	return u, nil
}

// issueSession creates the session row and the result the HTTP layer needs.
func (s *Service) issueSession(ctx context.Context, u *user.User, meta LoginMeta) (*LoginResult, error) {
	now := s.now()
	raw, hash, err := NewSessionToken()
	if err != nil {
		return nil, err
	}
	csrf, err := NewCSRFToken()
	if err != nil {
		return nil, err
	}

	session := CreateSessionParams{
		ID:        uuid.New(),
		UserID:    u.ID,
		TokenHash: hash,
		CSRFToken: csrf,
		IssuedAt:  now,
		// Fixed expiry, computed once and never extended (see migrations/0003).
		ExpiresAt: now.Add(s.cfg.SessionTTL),
		UserAgent: truncateUserAgent(meta.UserAgent),
		IP:        meta.IP,
	}
	created, err := s.sessions.Create(ctx, session)
	if err != nil {
		return nil, err
	}

	// Housekeeping, best effort: Phase 1 has no background job, and an expired
	// row is harmless (it cannot authenticate anything), so a failure here is
	// logged and ignored rather than turning a valid login into an error.
	if err := s.sessions.DeleteExpired(ctx, u.ID); err != nil {
		logging.FromContext(ctx).Debug("session cleanup failed", "error", err, logging.FieldUserID, u.ID.String())
	}
	// last_login_at is bookkeeping for the admin UI, not an authorization input.
	if err := s.users.TouchLastLogin(ctx, u.ID); err != nil {
		logging.FromContext(ctx).Warn("failed to record last_login_at",
			"error", err, logging.FieldUserID, u.ID.String())
	}
	u.LastLoginAt = &now

	logging.FromContext(ctx).Info("login succeeded",
		logging.FieldUserID, u.ID.String(),
		logging.FieldRole, string(u.Role),
		logging.FieldSessionID, created.ID.String(),
		"expires_at", created.ExpiresAt.UTC().Format(time.RFC3339),
	)

	return &LoginResult{
		Principal: Principal{
			UserID:      u.ID,
			Account:     u.Account,
			DisplayName: u.DisplayName,
			Role:        u.Role,
			Status:      u.Status,
			CreatedAt:   u.CreatedAt,
			LastLoginAt: u.LastLoginAt,
			SessionID:   created.ID,
			CSRFToken:   csrf,
		},
		User:         u,
		SessionToken: raw,
		CSRFToken:    csrf,
		ExpiresAt:    created.ExpiresAt,
	}, nil
}

// touchIfIdle refreshes last_seen_at, but at most once per IdleTouchInterval.
//
// WHY the throttle: last_seen_at is operational visibility, not an authorization
// input, and a teacher console polling every few seconds would otherwise turn
// each poll into an UPDATE on the session row (§41 — activity never extends
// expires_at, so there is nothing security-relevant to keep exactly current).
func (s *Service) touchIfIdle(ctx context.Context, sw *SessionWithUser, now time.Time) {
	interval := s.cfg.IdleTouchInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if now.Sub(sw.LastSeenAt) < interval {
		return
	}
	if err := s.sessions.Touch(ctx, sw.ID, now); err != nil {
		logging.FromContext(ctx).Debug("session touch failed",
			"error", err, logging.FieldSessionID, sw.ID.String())
	}
}

// rehash upgrades a stored hash to the current parameters after a successful
// verification.
func (s *Service) rehash(ctx context.Context, u *user.User, password string) {
	encoded, err := Hash(password)
	if err != nil {
		logging.FromContext(ctx).Warn("password rehash failed",
			"error", err, logging.FieldUserID, u.ID.String())
		return
	}
	if err := s.users.SetPasswordHash(ctx, u.ID, encoded); err != nil {
		logging.FromContext(ctx).Warn("storing rehashed password failed",
			"error", err, logging.FieldUserID, u.ID.String())
		return
	}
	u.PasswordHash = &encoded
	logging.FromContext(ctx).Info("password hash upgraded to current parameters",
		logging.FieldUserID, u.ID.String(), logging.FieldRole, string(u.Role))
}

// logLoginRejected records a failed login server-side.
//
// The reason is logged in full because that is where the anti-enumeration promise
// stops: operators need to see a brute-force attempt in progress, and the client
// is told nothing beyond "invalid credentials". The account is logged for unknown
// accounts too (it is an identifier, not a credential) — that is how an operator
// learns which account is under attack. The password never appears, at any level.
func logLoginRejected(ctx context.Context, role user.Role, u *user.User, reason string) {
	attrs := []any{logging.FieldRole, string(role), "reason", reason}
	if u != nil {
		attrs = append(attrs, logging.FieldUserID, u.ID.String())
	}
	logging.FromContext(ctx).Info("login rejected", attrs...)
}

// truncateUserAgent bounds the stored header without mangling it beyond
// recognition.
func truncateUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if len(ua) <= maxUserAgentLength {
		return ua
	}
	// Cut on a rune boundary so the stored value stays valid UTF-8.
	truncated := ua[:maxUserAgentLength]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}
