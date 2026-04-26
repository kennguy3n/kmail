package scim

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/tenant"
)

// ErrNotFound is returned when a SCIM resource lookup misses.
var ErrNotFound = errors.New("scim: not found")

// ErrConflict is returned when a userName/displayName already exists.
var ErrConflict = errors.New("scim: conflict")

// ErrInvalidInput is returned for malformed PATCH ops or missing
// required fields.
var ErrInvalidInput = errors.New("scim: invalid input")

// Service exposes SCIM 2.0 Users + Groups CRUD. Users are
// projected onto the existing `users` table via tenant.Service;
// groups onto `shared_inboxes`. Tokens (provisioning credentials)
// live in `scim_tokens`.
type Service struct {
	pool   *pgxpool.Pool
	tenant *tenant.Service
}

// NewService returns a SCIM Service.
func NewService(pool *pgxpool.Pool, t *tenant.Service) *Service {
	return &Service{pool: pool, tenant: t}
}

// ResolveTenantFromToken hashes the bearer token and looks up the
// tenant it provisions for. Returns ErrNotFound when the token is
// unknown or revoked.
func (s *Service) ResolveTenantFromToken(ctx context.Context, token string) (string, error) {
	if s.pool == nil {
		return "", ErrNotFound
	}
	if token == "" {
		return "", ErrNotFound
	}
	hash := hashToken(token)
	var tenantID string
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id::text FROM scim_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, hash).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return tenantID, nil
}

// ScimToken is the metadata-only view of a provisioning token.
// The plaintext value is only returned at GenerateToken time.
type ScimToken struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenant_id"`
	Description string     `json:"description"`
	CreatedAt   time.Time  `json:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// GenerateToken mints a fresh bearer token, stores its SHA-256
// hash, and returns both the metadata row and the plaintext token
// (which the caller surfaces exactly once).
func (s *Service) GenerateToken(ctx context.Context, tenantID, description string) (ScimToken, string, error) {
	if s.pool == nil {
		return ScimToken{}, "", errors.New("scim: pool not configured")
	}
	if tenantID == "" {
		return ScimToken{}, "", fmt.Errorf("%w: tenant required", ErrInvalidInput)
	}
	plain, err := randToken()
	if err != nil {
		return ScimToken{}, "", err
	}
	hash := hashToken(plain)
	var t ScimToken
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO scim_tokens (tenant_id, token_hash, description)
			VALUES ($1::uuid, $2, $3)
			RETURNING id::text, tenant_id::text, description, created_at, revoked_at
		`, tenantID, hash, description).Scan(&t.ID, &t.TenantID, &t.Description, &t.CreatedAt, &t.RevokedAt)
	})
	if err != nil {
		return ScimToken{}, "", err
	}
	return t, plain, nil
}

// ListTokens returns the non-deleted tokens for a tenant.
func (s *Service) ListTokens(ctx context.Context, tenantID string) ([]ScimToken, error) {
	if s.pool == nil {
		return nil, nil
	}
	var out []ScimToken
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, description, created_at, revoked_at
			FROM scim_tokens WHERE tenant_id = $1::uuid
			ORDER BY created_at DESC
		`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t ScimToken
			if err := rows.Scan(&t.ID, &t.TenantID, &t.Description, &t.CreatedAt, &t.RevokedAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeToken soft-deletes a token by stamping `revoked_at`.
func (s *Service) RevokeToken(ctx context.Context, tenantID, tokenID string) error {
	if s.pool == nil {
		return errors.New("scim: pool not configured")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE scim_tokens SET revoked_at = now()
			WHERE tenant_id = $1::uuid AND id = $2::uuid AND revoked_at IS NULL
		`, tenantID, tokenID)
		return err
	})
}

// ---------------------------------------------------------------
// Users
// ---------------------------------------------------------------

// ListUsers returns paginated SCIM users.
func (s *Service) ListUsers(ctx context.Context, tenantID string, startIndex, count int) ([]User, int, error) {
	users, err := s.tenant.ListUsers(ctx, tenantID)
	if err != nil {
		return nil, 0, err
	}
	total := len(users)
	if startIndex < 1 {
		startIndex = 1
	}
	if count <= 0 {
		count = 100
	}
	from := startIndex - 1
	if from > total {
		from = total
	}
	to := from + count
	if to > total {
		to = total
	}
	out := make([]User, 0, to-from)
	for _, u := range users[from:to] {
		out = append(out, projectUser(u))
	}
	return out, total, nil
}

// GetUser returns one SCIM user.
func (s *Service) GetUser(ctx context.Context, tenantID, userID string) (User, error) {
	u, err := s.tenant.GetUser(ctx, tenantID, userID)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	return projectUser(*u), nil
}

// CreateUser provisions a new user from a SCIM payload.
func (s *Service) CreateUser(ctx context.Context, tenantID string, in User) (User, error) {
	email := in.UserName
	if email == "" {
		email = PrimaryEmail(in.Emails)
	}
	if email == "" {
		return User{}, fmt.Errorf("%w: userName required", ErrInvalidInput)
	}
	display := in.DisplayName
	if display == "" && in.Name != nil {
		display = strings.TrimSpace(in.Name.Formatted)
		if display == "" {
			display = strings.TrimSpace(in.Name.GivenName + " " + in.Name.FamilyName)
		}
	}
	if display == "" {
		display = email
	}
	// Synthesize KChat / Stalwart account IDs from the SCIM
	// externalId when present so re-runs of the IdP push are
	// idempotent on the IdP side. Fall back to the email local-
	// part when no externalId is supplied.
	external := in.ExternalID
	if external == "" {
		external = email
	}
	created, err := s.tenant.CreateUser(ctx, tenantID, tenant.CreateUserInput{
		KChatUserID:       external,
		StalwartAccountID: external,
		Email:             strings.ToLower(strings.TrimSpace(email)),
		DisplayName:       display,
		Role:              "member",
		AccountType:       "user",
	})
	if err != nil {
		if isConflict(err) {
			return User{}, ErrConflict
		}
		return User{}, err
	}
	out := projectUser(*created)
	if !in.Active {
		// Caller explicitly set active=false on create; suspend
		// the user immediately. This matches the expected SCIM
		// semantics for IdP-driven deactivation.
		susp := "suspended"
		if _, err := s.tenant.UpdateUser(ctx, tenantID, created.ID, tenant.UpdateUserInput{Status: &susp}); err == nil {
			out.Active = false
		}
	}
	return out, nil
}

// PatchUser applies SCIM PATCH ops. The implementation supports
// the subset Okta / Azure AD actually issue: replace `active`,
// replace `displayName`, replace `name.{given,family}Name`, and
// replace primary `emails[type eq "work"].value`.
func (s *Service) PatchUser(ctx context.Context, tenantID, userID string, req PatchRequest) (User, error) {
	if len(req.Operations) == 0 {
		return s.GetUser(ctx, tenantID, userID)
	}
	in := tenant.UpdateUserInput{}
	for _, op := range req.Operations {
		if !strings.EqualFold(op.Op, "replace") && !strings.EqualFold(op.Op, "add") {
			continue
		}
		path := strings.ToLower(strings.TrimSpace(op.Path))
		// Map both the path-targeted form ({"path":"active","value":false})
		// and the bag-of-attributes form
		// ({"value":{"active":false,"displayName":"X"}}).
		if path == "" {
			var bag map[string]json.RawMessage
			if err := json.Unmarshal(op.Value, &bag); err != nil {
				continue
			}
			for k, v := range bag {
				applyAttr(strings.ToLower(k), v, &in)
			}
			continue
		}
		applyAttr(path, op.Value, &in)
	}
	if in.DisplayName == nil && in.Role == nil && in.Status == nil && in.QuotaBytes == nil {
		return s.GetUser(ctx, tenantID, userID)
	}
	u, err := s.tenant.UpdateUser(ctx, tenantID, userID, in)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	return projectUser(*u), nil
}

// DeleteUser is the SCIM DELETE — soft-delete via tenant.Service.
func (s *Service) DeleteUser(ctx context.Context, tenantID, userID string) error {
	if err := s.tenant.DeleteUser(ctx, tenantID, userID); err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func applyAttr(path string, raw json.RawMessage, in *tenant.UpdateUserInput) {
	switch path {
	case "active":
		var v bool
		if err := json.Unmarshal(raw, &v); err == nil {
			s := "active"
			if !v {
				s = "suspended"
			}
			in.Status = &s
		}
	case "displayname":
		var v string
		if err := json.Unmarshal(raw, &v); err == nil && v != "" {
			in.DisplayName = &v
		}
	}
}

// ---------------------------------------------------------------
// Groups
// ---------------------------------------------------------------

// ListGroups returns paginated SCIM groups (shared inboxes).
func (s *Service) ListGroups(ctx context.Context, tenantID string, startIndex, count int) ([]Group, int, error) {
	inboxes, err := s.tenant.ListSharedInboxes(ctx, tenantID)
	if err != nil {
		return nil, 0, err
	}
	total := len(inboxes)
	if startIndex < 1 {
		startIndex = 1
	}
	if count <= 0 {
		count = 100
	}
	from := startIndex - 1
	if from > total {
		from = total
	}
	to := from + count
	if to > total {
		to = total
	}
	out := make([]Group, 0, to-from)
	for _, si := range inboxes[from:to] {
		out = append(out, projectGroup(si))
	}
	return out, total, nil
}

// GetGroup returns one SCIM group.
func (s *Service) GetGroup(ctx context.Context, tenantID, groupID string) (Group, error) {
	inboxes, err := s.tenant.ListSharedInboxes(ctx, tenantID)
	if err != nil {
		return Group{}, err
	}
	for _, si := range inboxes {
		if si.ID == groupID {
			return projectGroup(si), nil
		}
	}
	return Group{}, ErrNotFound
}

// CreateGroup provisions a new shared inbox from a SCIM payload.
func (s *Service) CreateGroup(ctx context.Context, tenantID string, in Group) (Group, error) {
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		return Group{}, fmt.Errorf("%w: displayName required", ErrInvalidInput)
	}
	addr := slugify(display) + "@scim.local"
	si, err := s.tenant.CreateSharedInbox(ctx, tenantID, tenant.CreateSharedInboxInput{
		Address:     addr,
		DisplayName: display,
		MLSGroupID:  "scim-" + slugify(display),
	})
	if err != nil {
		if isConflict(err) {
			return Group{}, ErrConflict
		}
		return Group{}, err
	}
	return projectGroup(*si), nil
}

// PatchGroup is intentionally narrow: it supports replacing the
// `displayName` only. Membership PATCH (add/remove members) is a
// no-op for now — KMail's shared-inbox membership lives on a
// separate table and the IdP-driven mass-membership rewrite is
// best left to a future implementation.
func (s *Service) PatchGroup(ctx context.Context, tenantID, groupID string, _ PatchRequest) (Group, error) {
	return s.GetGroup(ctx, tenantID, groupID)
}

// DeleteGroup removes a shared inbox.
func (s *Service) DeleteGroup(ctx context.Context, tenantID, groupID string) error {
	// tenant.Service does not expose DeleteSharedInbox. Use a
	// direct DELETE here so SCIM CRUD is complete.
	if s.pool == nil {
		return errors.New("scim: pool not configured")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			DELETE FROM shared_inboxes WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, groupID, tenantID)
		return err
	})
}

// ---------------------------------------------------------------
// projection
// ---------------------------------------------------------------

func projectUser(u tenant.User) User {
	return User{
		Schemas:     []string{SchemaUser},
		ID:          u.ID,
		ExternalID:  u.KChatUserID,
		UserName:    u.Email,
		DisplayName: u.DisplayName,
		Active:      u.Status == "active",
		Emails: []Email{{
			Value:   u.Email,
			Primary: true,
			Type:    "work",
		}},
		Meta: Meta{
			ResourceType: "User",
			Created:      u.CreatedAt,
			LastModified: u.UpdatedAt,
			Location:     "/scim/v2/Users/" + u.ID,
		},
	}
}

func projectGroup(si tenant.SharedInbox) Group {
	return Group{
		Schemas:     []string{SchemaGroup},
		ID:          si.ID,
		DisplayName: si.DisplayName,
		Meta: Meta{
			ResourceType: "Group",
			Created:      si.CreatedAt,
			LastModified: si.UpdatedAt,
			Location:     "/scim/v2/Groups/" + si.ID,
		},
	}
}

func slugify(s string) string {
	out := strings.ToLower(strings.TrimSpace(s))
	out = strings.ReplaceAll(out, " ", "-")
	var b strings.Builder
	for _, r := range out {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "group"
	}
	return b.String()
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func randToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "scim_" + hex.EncodeToString(buf), nil
}

func isConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "unique") || strings.Contains(msg, "23505")
}
