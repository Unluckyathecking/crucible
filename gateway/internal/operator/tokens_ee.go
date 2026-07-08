// Crucible Enterprise Edition (EE) file.
// Licensed under the Crucible Enterprise License — see ee/LICENSE.md.
// Not covered by the repository's MIT license.

package operator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/license"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

// tokenPrefix distinguishes operator tokens from customer API keys (cru_live_…)
// at a glance in logs and dashboards. The random suffix uses the same RFC 4648
// base32 (no padding) alphabet as auth.Generate.
const tokenPrefix = "opt_"

// Token is the operator-visible projection of an operator_tokens row. The hash
// column is never selected into it — token material only ever leaves the system
// once, in the Create response.
type Token struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// TokenStore manages DB-backed operator tokens. Unlike Store it is not
// SELECT-only: Create/Revoke are the operator's own credential lifecycle, gated
// by the deployment license at the call sites (see tokens_handlers_ee.go and
// TokenMiddleware). It never touches customer, plan, key, or billing state.
type TokenStore struct {
	db *pgxpool.Pool
}

// NewTokenStore returns a TokenStore backed by db.
func NewTokenStore(db *pgxpool.Pool) *TokenStore {
	return &TokenStore{db: db}
}

// Create mints a new operator token, stores only SHA-256(salt || token), and
// returns the row metadata plus the full token string. The full token is the
// only time the plaintext exists outside the caller — it is shown to the
// operator exactly once, never persisted, and never recoverable.
func (s *TokenStore) Create(ctx context.Context, name, salt string) (Token, string, error) {
	full, err := generateToken()
	if err != nil {
		return Token{}, "", err
	}
	hash := hashToken(salt, full)

	var t Token
	err = s.db.QueryRow(ctx, `
		INSERT INTO operator_tokens (name, hash)
		VALUES ($1, $2)
		RETURNING id, name, created_at, revoked_at
	`, name, hash).Scan(&t.ID, &t.Name, &t.CreatedAt, &t.RevokedAt)
	if err != nil {
		return Token{}, "", err
	}
	return t, full, nil
}

// TokensFilter constrains the List query.
type TokensFilter struct {
	Page    int
	PerPage int
}

func (f *TokensFilter) normalize() {
	f.Page, f.PerPage = paging.Clamp(f.Page, f.PerPage, 20, 100)
}

// List returns a paginated view of operator tokens, newest first. It selects
// only non-secret columns — the hash is never read back.
func (s *TokenStore) List(ctx context.Context, f TokensFilter) (paging.Page[Token], error) {
	f.normalize()
	offset, err := paging.Offset(f.Page, f.PerPage)
	if err != nil {
		return paging.Page[Token]{}, err
	}

	var total int64
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM operator_tokens`).Scan(&total); err != nil {
		return paging.Page[Token]{}, err
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, name, created_at, revoked_at
		FROM operator_tokens
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`, f.PerPage, offset)
	if err != nil {
		return paging.Page[Token]{}, err
	}
	defer rows.Close()

	items := []Token{}
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.RevokedAt); err != nil {
			return paging.Page[Token]{}, err
		}
		items = append(items, t)
	}
	if err := rows.Err(); err != nil {
		return paging.Page[Token]{}, err
	}
	return paging.Page[Token]{Items: items, Total: total}, nil
}

// Revoke sets revoked_at on the token, taking effect on the very next request
// (there is no auth cache for operator tokens). It reports whether the token
// exists at all so the handler can 404 an unknown id; re-revoking an already
// revoked token is idempotent and still reports found.
func (s *TokenStore) Revoke(ctx context.Context, id uuid.UUID) (found bool, err error) {
	ct, err := s.db.Exec(ctx, `
		UPDATE operator_tokens SET revoked_at = NOW()
		WHERE id = $1 AND revoked_at IS NULL
	`, id)
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() > 0 {
		return true, nil
	}
	// Zero rows updated: either the id is unknown or it was already revoked.
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_tokens WHERE id = $1)`, id).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// Verify reports whether presented matches any non-revoked operator token.
// Every comparison uses subtle.ConstantTimeCompare over the stored SHA-256 hash;
// a full scan of the non-revoked set is used deliberately (operator token counts
// are tiny, < 100) so no prefix column or extra migration is needed. The loop
// checks every candidate without early exit so match position cannot be inferred
// from timing.
func (s *TokenStore) Verify(ctx context.Context, salt, presented string) (bool, error) {
	if presented == "" {
		return false, nil
	}
	want := hashToken(salt, presented)

	rows, err := s.db.Query(ctx, `SELECT hash FROM operator_tokens WHERE revoked_at IS NULL`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	matched := false
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return false, err
		}
		if subtle.ConstantTimeCompare(h, want) == 1 {
			matched = true
		}
	}
	return matched, rows.Err()
}

// hashToken returns SHA-256(salt || token), stored as operator_tokens.hash. This
// mirrors auth.Hash's salted-hash construction (the plaintext token is never
// persisted); it is reimplemented here rather than imported to avoid an
// operator→auth→webhookout→operator test import cycle.
func hashToken(salt, token string) []byte {
	h := sha256.New()
	h.Write([]byte(salt))
	h.Write([]byte(token))
	return h.Sum(nil)
}

// generateToken returns a fresh operator token: the "opt_" prefix followed by
// 192 bits of base32-encoded entropy (same alphabet as customer API keys).
func generateToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return tokenPrefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// TokenMiddleware gates the /v1/admin subrouter with both the static
// OPERATOR_TOKEN (the community path, always accepted) and — only when the
// deployment license grants FeatureOperatorTokens — DB-backed operator tokens.
// When the license is absent (community edition), lic.Has is false and the DB is
// never consulted, so behaviour is byte-identical to the static-only Middleware.
func TokenMiddleware(staticToken string, tokens *TokenStore, salt string, lic *license.License) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
			bearer := parseBearer(r.Header.Get("Authorization"))

			if staticTokenMatches(staticToken, bearer) {
				next.ServeHTTP(w, r)
				return
			}

			if bearer != "" && tokens != nil && lic.Has(license.FeatureOperatorTokens) {
				ok, err := tokens.Verify(r.Context(), salt, bearer)
				if err == nil && ok {
					next.ServeHTTP(w, r)
					return
				}
			}

			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "invalid operator token", false)
		})
	}
}
