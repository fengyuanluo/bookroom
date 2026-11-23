// Package auth owns accounts, passwords, access/refresh tokens and per-device sync keys.
package auth

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"bookroom/internal/apperr"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// HashParams are the Argon2id parameters for new password hashes. Tests lower them.
var HashParams = argon2id.DefaultParams

// User is an account as the rest of the server sees it.
type User struct {
	ID        string
	Username  string
	Email     string
	IsAdmin   bool
	CreatedAt time.Time
}

// Tokens is a freshly issued access/refresh pair.
type Tokens struct {
	Access    string
	Refresh   string
	ExpiresAt time.Time
	ExpiresIn int
	User      User
}

// Device is a sync key holder; the key itself is only known at creation.
type Device struct {
	ID         string
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// UserHook runs inside the transaction that creates a user (the library projects itself into the new
// account's Readest rows here).
type UserHook func(ctx context.Context, q *db.Queries, userID string, now int64) error

// Service implements every account operation.
type Service struct {
	st         *store.Store
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	onCreate   UserHook

	basicMu    sync.Mutex
	basicCache map[[32]byte]basicEntry
}

type basicEntry struct {
	userID  string
	expires time.Time
}

const basicCacheTTL = 5 * time.Minute

// New builds the service. onCreate may be nil.
func New(st *store.Store, secret string, accessTTL, refreshTTL time.Duration, onCreate UserHook) *Service {
	return &Service{
		st: st, secret: []byte(secret), accessTTL: accessTTL, refreshTTL: refreshTTL, onCreate: onCreate,
		basicCache: map[[32]byte]basicEntry{},
	}
}

func toUser(id, username string, email *string, admin int64, created int64) User {
	u := User{ID: id, Username: username, IsAdmin: admin != 0, CreatedAt: time.UnixMilli(created).UTC()}
	if email != nil {
		u.Email = *email
	}
	return u
}

func fromRow(r db.User) User { return toUser(r.ID, r.Username, r.Email, r.IsAdmin, r.CreatedAt) }

// CreateUser adds an account. Usernames are case-insensitive and may not contain ':' or whitespace (KOSync
// and HTTP Basic both reserve ':').
func (s *Service) CreateUser(ctx context.Context, username, password, email string, admin bool) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" || strings.ContainsAny(username, ": \t\r\n") {
		return User{}, apperr.BadRequest("username must be non-empty without ':' or whitespace")
	}
	if len(password) < 8 {
		return User{}, apperr.BadRequest("password must be at least 8 characters")
	}
	hash, err := argon2id.CreateHash(password, HashParams)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UnixMilli()
	row := db.User{ID: uuid.NewString(), Username: username, PasswordHash: hash, CreatedAt: now, UpdatedAt: now}
	if admin {
		row.IsAdmin = 1
	}
	if email = strings.TrimSpace(email); email != "" {
		row.Email = &email
	}
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.GetUserByLogin(ctx, username); err == nil {
			return apperr.Conflict("username is taken")
		}
		if row.Email != nil {
			if _, err := q.GetUserByLogin(ctx, email); err == nil {
				return apperr.Conflict("email is taken")
			}
		}
		if err := q.CreateUser(ctx, db.CreateUserParams{
			ID: row.ID, Username: row.Username, Email: row.Email, PasswordHash: row.PasswordHash,
			IsAdmin: row.IsAdmin, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		if s.onCreate != nil {
			return s.onCreate(ctx, q, row.ID, now)
		}
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return fromRow(row), nil
}

// EnsureAdmin creates the bootstrap admin when the database has no users. It reports whether it did.
func (s *Service) EnsureAdmin(ctx context.Context, username, password string) (bool, error) {
	n, err := s.st.Q.CountUsers(ctx)
	if err != nil || n > 0 || username == "" {
		return false, err
	}
	_, err = s.CreateUser(ctx, username, password, "", true)
	return err == nil, err
}

// User loads an account.
func (s *Service) User(ctx context.Context, id string) (User, error) {
	r, err := s.st.Q.GetUser(ctx, id)
	if store.IsNotFound(err) {
		return User{}, apperr.NotFound("no such user")
	}
	if err != nil {
		return User{}, err
	}
	return fromRow(r), nil
}

// ListUsers returns every account.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.st.Q.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, nil
}

// DeleteUser removes an account with all of its sync data (ON DELETE CASCADE).
func (s *Service) DeleteUser(ctx context.Context, actorID, id string) error {
	if actorID == id {
		return apperr.BadRequest("an admin cannot delete itself")
	}
	n, err := s.st.WQ.DeleteUser(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("no such user")
	}
	s.clearBasicCache()
	return nil
}

// Authenticate checks a login (username or email) and password.
func (s *Service) Authenticate(ctx context.Context, login, password string) (User, error) {
	r, err := s.st.Q.GetUserByLogin(ctx, strings.TrimSpace(login))
	if store.IsNotFound(err) {
		// Spend comparable time so response timing does not reveal which usernames exist.
		_, _ = argon2id.ComparePasswordAndHash(password, dummyHash)
		return User{}, apperr.Unauthorized("invalid username or password")
	}
	if err != nil {
		return User{}, err
	}
	ok, err := argon2id.ComparePasswordAndHash(password, r.PasswordHash)
	if err != nil || !ok {
		return User{}, apperr.Unauthorized("invalid username or password")
	}
	return fromRow(r), nil
}

var dummyHash = func() string {
	h, _ := argon2id.CreateHash("bookroom-timing-equalizer", argon2id.DefaultParams)
	return h
}()

// Login authenticates and issues a token pair.
func (s *Service) Login(ctx context.Context, login, password string) (Tokens, error) {
	u, err := s.Authenticate(ctx, login, password)
	if err != nil {
		return Tokens{}, err
	}
	return s.issue(ctx, s.st.WQ, u)
}

// Refresh consumes a refresh token and issues a new pair.
func (s *Service) Refresh(ctx context.Context, refresh string) (Tokens, error) {
	var out Tokens
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		row, err := q.TakeRefreshToken(ctx, hashToken(refresh))
		if store.IsNotFound(err) {
			return apperr.Unauthorized("refresh token is invalid or already used")
		}
		if err != nil {
			return err
		}
		if row.ExpiresAt < time.Now().UnixMilli() {
			return apperr.Unauthorized("refresh token expired")
		}
		ur, err := q.GetUser(ctx, row.UserID)
		if err != nil {
			return apperr.Unauthorized("user no longer exists")
		}
		out, err = s.issue(ctx, q, fromRow(ur))
		return err
	})
	return out, err
}

// Logout revokes one refresh token.
func (s *Service) Logout(ctx context.Context, refresh string) error {
	return s.st.WQ.DeleteRefreshToken(ctx, hashToken(refresh))
}

// RevokeAll revokes every refresh token of a user.
func (s *Service) RevokeAll(ctx context.Context, userID string) error {
	return s.st.WQ.DeleteUserRefreshTokens(ctx, userID)
}

func (s *Service) issue(ctx context.Context, q *db.Queries, u User) (Tokens, error) {
	now := time.Now()
	exp := now.Add(s.accessTTL)
	claims := jwt.MapClaims{
		"iss": "bookroom", "sub": u.ID, "aud": "authenticated", "role": "authenticated",
		"iat": now.Unix(), "exp": exp.Unix(), "adm": u.IsAdmin, "name": u.Username,
	}
	if u.Email != "" {
		claims["email"] = u.Email
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return Tokens{}, err
	}
	refresh := randomToken(32)
	if err := q.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		TokenHash: hashToken(refresh), UserID: u.ID,
		ExpiresAt: now.Add(s.refreshTTL).UnixMilli(), CreatedAt: now.UnixMilli(),
	}); err != nil {
		return Tokens{}, err
	}
	return Tokens{Access: access, Refresh: refresh, ExpiresAt: exp.UTC(), ExpiresIn: int(s.accessTTL.Seconds()), User: u}, nil
}

// VerifyAccess validates an access token and returns the current account (so deleted users and revoked
// admin rights take effect immediately).
func (s *Service) VerifyAccess(ctx context.Context, token string) (User, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("bookroom"), jwt.WithExpirationRequired())
	if err != nil {
		return User{}, apperr.Unauthorized("invalid or expired access token")
	}
	sub, err := parsed.Claims.GetSubject()
	if err != nil || sub == "" {
		return User{}, apperr.Unauthorized("token has no subject")
	}
	u, err := s.User(ctx, sub)
	if err != nil {
		return User{}, apperr.Unauthorized("user no longer exists")
	}
	return u, nil
}

// ChangePassword verifies the current password, sets a new one and revokes refresh tokens.
func (s *Service) ChangePassword(ctx context.Context, userID, current, next string) error {
	u, err := s.User(ctx, userID)
	if err != nil {
		return err
	}
	if _, err := s.Authenticate(ctx, u.Username, current); err != nil {
		return apperr.BadRequest("current password is wrong")
	}
	return s.SetPassword(ctx, userID, next)
}

// SetPassword replaces a password without checking the old one (admin reset).
func (s *Service) SetPassword(ctx context.Context, userID, password string) error {
	if len(password) < 8 {
		return apperr.BadRequest("password must be at least 8 characters")
	}
	hash, err := argon2id.CreateHash(password, HashParams)
	if err != nil {
		return err
	}
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		n, err := q.SetUserPassword(ctx, db.SetUserPasswordParams{PasswordHash: hash, UpdatedAt: time.Now().UnixMilli(), ID: userID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.NotFound("no such user")
		}
		return q.DeleteUserRefreshTokens(ctx, userID)
	})
	if err == nil {
		s.clearBasicCache()
	}
	return err
}

// CreateDevice issues a device key. The key is returned once; only sha256(md5hex(key)) is stored.
func (s *Service) CreateDevice(ctx context.Context, userID, name string) (Device, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Device{}, "", apperr.BadRequest("device name is required")
	}
	key := newDeviceKey()
	now := time.Now()
	d := Device{ID: uuid.NewString(), Name: name, CreatedAt: now.UTC()}
	err := s.st.WQ.CreateDevice(ctx, db.CreateDeviceParams{
		ID: d.ID, UserID: userID, Name: name, KeyHash: DeviceKeyHash(MD5Hex(key)), CreatedAt: now.UnixMilli(),
	})
	return d, key, err
}

// ListDevices returns a user's devices.
func (s *Service) ListDevices(ctx context.Context, userID string) ([]Device, error) {
	rows, err := s.st.Q.ListDevices(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]Device, len(rows))
	for i, r := range rows {
		out[i] = Device{ID: r.ID, Name: r.Name, CreatedAt: time.UnixMilli(r.CreatedAt).UTC()}
		if r.LastUsedAt != nil {
			out[i].LastUsedAt = new(time.UnixMilli(*r.LastUsedAt).UTC())
		}
	}
	return out, nil
}

// DeleteDevice revokes a key.
func (s *Service) DeleteDevice(ctx context.Context, userID, id string) error {
	n, err := s.st.WQ.DeleteDevice(ctx, db.DeleteDeviceParams{ID: id, UserID: userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("no such device")
	}
	s.clearBasicCache()
	return nil
}

// UserByDeviceKeyMD5 resolves the x-auth-key a KOSync client sends (lowercase hex MD5 of the key).
func (s *Service) UserByDeviceKeyMD5(ctx context.Context, keyMD5 string) (User, error) {
	row, err := s.st.Q.GetDeviceUser(ctx, DeviceKeyHash(strings.ToLower(strings.TrimSpace(keyMD5))))
	if store.IsNotFound(err) {
		return User{}, apperr.Unauthorized("unknown device key")
	}
	if err != nil {
		return User{}, err
	}
	// Last-use bookkeeping must not slow the sync path or fail it.
	go func(id string) {
		_ = s.st.WQ.TouchDevice(context.Background(), db.TouchDeviceParams{LastUsedAt: new(time.Now().UnixMilli()), ID: id})
	}(row.DeviceID)
	return toUser(row.ID, row.Username, row.Email, row.IsAdmin, row.CreatedAt), nil
}

// Basic authenticates HTTP Basic credentials (OPDS): the password may be a device key or the account
// password. Successful password checks are cached briefly because Argon2id is deliberately slow and OPDS
// clients authenticate every cover request.
func (s *Service) Basic(ctx context.Context, username, password string) (User, error) {
	if username == "" || password == "" {
		return User{}, apperr.Unauthorized("credentials required")
	}
	if u, err := s.UserByDeviceKeyMD5(ctx, MD5Hex(password)); err == nil {
		return u, nil
	}
	key := sha256.Sum256([]byte(username + "\x00" + password))
	s.basicMu.Lock()
	e, ok := s.basicCache[key]
	s.basicMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return s.User(ctx, e.userID)
	}
	u, err := s.Authenticate(ctx, username, password)
	if err != nil {
		return User{}, err
	}
	s.basicMu.Lock()
	if len(s.basicCache) > 1024 {
		clear(s.basicCache)
	}
	s.basicCache[key] = basicEntry{userID: u.ID, expires: time.Now().Add(basicCacheTTL)}
	s.basicMu.Unlock()
	return u, nil
}

func (s *Service) clearBasicCache() {
	s.basicMu.Lock()
	clear(s.basicCache)
	s.basicMu.Unlock()
}

// MD5Hex is lowercase hex MD5, the form KOSync clients send passwords in.
func MD5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// DeviceKeyHash is what the devices table stores for a key given as its MD5 hex.
func DeviceKeyHash(keyMD5 string) string {
	sum := sha256.Sum256([]byte(keyMD5))
	return hex.EncodeToString(sum[:])
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("auth: crypto/rand: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Device keys are typed on e-ink keyboards: 16 characters from an unambiguous lowercase alphabet in groups
// of four (80 bits).
const keyAlphabet = "23456789abcdefghjkmnpqrstuvwxyz"

func newDeviceKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("auth: crypto/rand: %w", err))
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(keyAlphabet[int(c)%len(keyAlphabet)])
	}
	return sb.String()
}
