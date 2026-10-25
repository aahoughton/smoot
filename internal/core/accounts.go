package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"strings"
	"time"
)

// Accounts, tokens and browser sessions exist for auth mode `token`. A token
// is shown once when issued; the database keeps only its SHA-256 hash. A
// browser session comes from a token pasted at login and ends when that
// token is revoked or expires, or at its own expiry.

// TokenPrefix starts every token, so a leaked one is recognizable.
const TokenPrefix = "smoot_"

// SessionPrefix starts every browser session secret.
const SessionPrefix = "smoots_"

// DefaultSessionTTL is how long a browser session lasts.
const DefaultSessionTTL = 30 * 24 * time.Hour

// lastUsedEvery limits how often authentication records a token's use.
const lastUsedEvery = time.Minute

// Account is a person or service that holds tokens.
type Account struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	ServerAdmin bool      `json:"server_admin"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// Token describes an issued token. The secret is not stored.
type Token struct {
	ID         int64      `json:"id"`
	Account    string     `json:"account"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// IssuedToken is a new token with its secret, which is shown only once.
type IssuedToken struct {
	Token
	Secret string `json:"secret"`
}

// Role is an account's standing in one namespace.
type Role string

// Roles, in increasing order of privilege.
const (
	RoleViewer   Role = "viewer"
	RoleObserver Role = "observer"
	RoleMember   Role = "member"
	RoleAdmin    Role = "admin"
)

// allows reports whether the role permits the action.
func (r Role) allows(a Action) bool {
	rank := map[Role]int{RoleViewer: 1, RoleObserver: 2, RoleMember: 3, RoleAdmin: 4}
	need := map[Action]int{ActionRead: 1, ActionObserve: 2, ActionParticipate: 3, ActionAdmin: 4}
	return rank[r] > 0 && rank[r] >= need[a]
}

func checkRole(r Role) error {
	switch r {
	case RoleViewer, RoleObserver, RoleMember, RoleAdmin:
		return nil
	}
	return errorf(CodeInvalid, "invalid role %q: want viewer, observer, member or admin", r)
}

// Membership is an account's role in a namespace.
type Membership struct {
	Namespace string `json:"namespace"`
	Account   string `json:"account"`
	Role      Role   `json:"role"`
}

// newSecret returns a random secret with the given prefix and its hash.
func newSecret(prefix string) (string, []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail
	secret := prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return secret, hashSecret(secret)
}

func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

func unauthorized(msg string) error { return errorf(CodeUnauthorized, "%s", msg) }

// AuthenticateToken returns the principal for a token secret.
func (s *Service) AuthenticateToken(ctx context.Context, secret string) (Principal, error) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return Principal{}, unauthorized("not a smoot token")
	}
	var id, account int64
	var lastUsed sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, account_id, last_used_at FROM tokens WHERE hash = ?`,
		hashSecret(secret)).Scan(&id, &account, &lastUsed)
	if isNoRows(err) {
		return Principal{}, unauthorized("unknown token")
	}
	if err != nil {
		return Principal{}, err
	}
	p := Principal{AccountID: account, Credential: Credential{Kind: CredentialToken, ID: id}}
	if err := s.checkCredential(ctx, s.db, p); err != nil {
		return Principal{}, err
	}
	now := s.now()
	if !lastUsed.Valid || now.Sub(fromMS(lastUsed.Int64)) >= lastUsedEvery {
		if _, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, ms(now), id); err != nil {
			return Principal{}, err
		}
	}
	return p, nil
}

// AuthenticateSession returns the principal for a browser session secret.
func (s *Service) AuthenticateSession(ctx context.Context, secret string) (Principal, error) {
	if !strings.HasPrefix(secret, SessionPrefix) {
		return Principal{}, unauthorized("not a smoot session")
	}
	var id, account int64
	hash := hashSecret(secret)
	err := s.db.QueryRowContext(ctx, `SELECT id, account_id FROM browser_sessions WHERE hash = ?`,
		hash).Scan(&id, &account)
	if isNoRows(err) {
		return Principal{}, unauthorized("unknown session")
	}
	if err != nil {
		return Principal{}, err
	}
	p := Principal{AccountID: account, Credential: Credential{Kind: CredentialSession, ID: id, hash: string(hash)}}
	return p, s.checkCredential(ctx, s.db, p)
}

// checkCredential reports whether p's credential is still valid and its
// account enabled: a CodeUnauthorized error otherwise.
func (s *Service) checkCredential(ctx context.Context, q querier, p Principal) error {
	now := ms(s.now())
	var ok bool
	var err error
	switch p.Credential.Kind {
	case CredentialToken:
		err = q.QueryRowContext(ctx, `SELECT count(*) FROM tokens t JOIN accounts a ON a.id = t.account_id
			WHERE t.id = ? AND t.account_id = ? AND t.revoked_at IS NULL
			AND (t.expires_at IS NULL OR t.expires_at > ?) AND a.disabled_at IS NULL`,
			p.Credential.ID, p.AccountID, now).Scan(&ok)
	case CredentialSession:
		err = q.QueryRowContext(ctx, `SELECT count(*) FROM browser_sessions s
			JOIN tokens t ON t.id = s.source_token_id JOIN accounts a ON a.id = s.account_id
			WHERE s.id = ? AND s.hash = ? AND s.account_id = ? AND s.revoked_at IS NULL AND s.expires_at > ?
			AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > ?) AND a.disabled_at IS NULL`,
			p.Credential.ID, []byte(p.Credential.hash), p.AccountID, now, now).Scan(&ok)
	default:
		return unauthorized("sign in with a token")
	}
	if err != nil {
		return err
	}
	if !ok {
		return unauthorized("credential revoked, expired or disabled")
	}
	return nil
}

// MaxSessionsPerAccount caps an account's browser sessions; signing in
// past it ends the oldest.
const MaxSessionsPerAccount = 32

// CreateSession starts a browser session for a principal authenticated by
// token, and returns its secret. The session ends with the token. If
// replaces is a session secret, that session is revoked in the same
// transaction, so a login either swaps the sessions or changes nothing.
// Signing in is a write for the account's write limit. It deletes the
// account's dead sessions (ended, expired, or from a token that no longer
// works) and, past MaxSessionsPerAccount, its oldest.
func (s *Service) CreateSession(ctx context.Context, p Principal, ttl time.Duration, replaces string) (string, time.Time, error) {
	if p.Credential.Kind != CredentialToken {
		return "", time.Time{}, unauthorized("sign in with a token")
	}
	if err := s.limitWrite(p); err != nil {
		return "", time.Time{}, err
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	secret, hash := newSecret(SessionPrefix)
	now := s.now()
	expires := now.Add(ttl)
	err := s.write(ctx, func(tx *wtx) error {
		if err := s.checkCredential(ctx, tx, p); err != nil {
			return err
		}
		if replaces != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE browser_sessions SET revoked_at = ?
				WHERE hash = ? AND revoked_at IS NULL`, ms(now), hashSecret(replaces)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM browser_sessions WHERE account_id = ?
			AND (revoked_at IS NOT NULL OR expires_at <= ? OR source_token_id IN
				(SELECT id FROM tokens WHERE revoked_at IS NOT NULL OR (expires_at IS NOT NULL AND expires_at <= ?)))`,
			p.AccountID, ms(now), ms(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO browser_sessions
			(account_id, source_token_id, hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
			p.AccountID, p.Credential.ID, hash, ms(now), ms(expires)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM browser_sessions WHERE account_id = ? AND id NOT IN
			(SELECT id FROM browser_sessions WHERE account_id = ? ORDER BY id DESC LIMIT ?)`,
			p.AccountID, p.AccountID, MaxSessionsPerAccount)
		return err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return secret, expires, nil
}

// EndSession revokes a browser session by its secret. An unknown secret is
// not an error.
func (s *Service) EndSession(ctx context.Context, secret string) error {
	return s.write(ctx, func(tx *wtx) error {
		_, err := tx.ExecContext(ctx, `UPDATE browser_sessions SET revoked_at = ?
			WHERE hash = ? AND revoked_at IS NULL`, ms(s.now()), hashSecret(secret))
		return err
	})
}

// Identity is what a principal is: its account and namespace roles.
type Identity struct {
	Account     *Account     `json:"account,omitempty"`
	Memberships []Membership `json:"memberships"`
}

// Whoami describes the principal. The anonymous principal has no account.
func (s *Service) Whoami(ctx context.Context, p Principal) (Identity, error) {
	id := Identity{Memberships: []Membership{}}
	if p.IsAnonymous() {
		return id, nil
	}
	if err := s.checkCredential(ctx, s.db, p); err != nil {
		return Identity{}, err
	}
	a, err := s.accountByID(ctx, s.db, p.AccountID)
	if err != nil {
		return Identity{}, err
	}
	id.Account = &a
	id.Memberships, err = collect(ctx, s.db, func(rows *sql.Rows) (Membership, error) {
		m := Membership{Account: a.Name}
		err := rows.Scan(&m.Namespace, &m.Role)
		return m, err
	}, `SELECT n.name, m.role FROM memberships m JOIN namespaces n ON n.id = m.namespace_id
		WHERE m.account_id = ? ORDER BY n.name`, p.AccountID)
	return id, err
}

const accountColumns = `id, name, server_admin, disabled_at IS NOT NULL, created_at`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var created int64
	err := row.Scan(&a.ID, &a.Name, &a.ServerAdmin, &a.Disabled, &created)
	a.CreatedAt = fromMS(created)
	return a, err
}

func (s *Service) accountByID(ctx context.Context, q querier, id int64) (Account, error) {
	a, err := scanAccount(q.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id))
	if isNoRows(err) {
		return Account{}, errorf(CodeAccountNotFound, "account %d not found", id)
	}
	return a, err
}

func (s *Service) accountByName(ctx context.Context, q querier, name string) (Account, error) {
	a, err := scanAccount(q.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts
		WHERE name = ? COLLATE NOCASE`, name))
	if isNoRows(err) {
		return Account{}, errorf(CodeAccountNotFound, "account %q not found", name)
	}
	return a, err
}

// requireServerAdmin allows only server admins.
func (s *Service) requireServerAdmin(ctx context.Context, p Principal) error {
	return s.authorize(ctx, p, "", ActionAdmin)
}

// requireSelfOrAdmin allows the account itself or a server admin.
func (s *Service) requireSelfOrAdmin(ctx context.Context, p Principal, a Account) error {
	return s.selfOrAdminIn(ctx, s.db, p, a.ID)
}

func (s *Service) selfOrAdminIn(ctx context.Context, q querier, p Principal, account int64) error {
	if !p.IsAnonymous() && p.AccountID == account {
		return s.checkCredential(ctx, q, p)
	}
	return s.authorizeIn(ctx, q, p, "", ActionAdmin)
}

// writeSelfOrAdmin is a write by the account itself or a server admin,
// checked again inside the transaction.
func (s *Service) writeSelfOrAdmin(ctx context.Context, p Principal, account int64, fn func(tx *wtx) error) error {
	return s.writeChecked(ctx, p, func(q querier) error { return s.selfOrAdminIn(ctx, q, p, account) }, fn)
}

func (s *Service) insertAccount(ctx context.Context, tx *wtx, name string, serverAdmin bool) (Account, error) {
	if err := checkName("account", name); err != nil {
		return Account{}, err
	}
	now := s.now()
	res, err := tx.ExecContext(ctx, `INSERT INTO accounts (name, server_admin, created_at) VALUES (?, ?, ?)`,
		name, serverAdmin, ms(now))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return Account{}, errorf(CodeAccountExists, "account %q already exists", name)
	}
	if err != nil {
		return Account{}, err
	}
	id, err := res.LastInsertId()
	return Account{ID: id, Name: name, ServerAdmin: serverAdmin, CreatedAt: fromMS(ms(now))}, err
}

func (s *Service) insertToken(ctx context.Context, tx *wtx, a Account, label string, ttl time.Duration) (IssuedToken, error) {
	if ttl < 0 {
		return IssuedToken{}, errorf(CodeInvalid, "token lifetime must not be negative")
	}
	if len(label) > MaxLabelBytes || !Printable(label) {
		return IssuedToken{}, errorf(CodeInvalid, "token label must be at most %d bytes of printable text", MaxLabelBytes)
	}
	secret, hash := newSecret(TokenPrefix)
	now := s.now()
	var expires sql.NullInt64
	if ttl > 0 {
		expires = sql.NullInt64{Int64: ms(now.Add(ttl)), Valid: true}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO tokens (account_id, hash, label, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, a.ID, hash, label, ms(now), expires)
	if err != nil {
		return IssuedToken{}, err
	}
	id, err := res.LastInsertId()
	return IssuedToken{Secret: secret, Token: Token{
		ID: id, Account: a.Name, Label: label, CreatedAt: fromMS(ms(now)), ExpiresAt: fromNullMS(expires),
	}}, err
}

// CreateAccount adds an account. Server admins only.
func (s *Service) CreateAccount(ctx context.Context, p Principal, name string, serverAdmin bool) (Account, error) {
	if err := s.requireServerAdmin(ctx, p); err != nil {
		return Account{}, err
	}
	if err := s.limitWrite(p); err != nil {
		return Account{}, err
	}
	var a Account
	err := s.writeAs(ctx, p, "", ActionAdmin, func(tx *wtx) error {
		var err error
		a, err = s.insertAccount(ctx, tx, name, serverAdmin)
		return err
	})
	return a, err
}

// ListAccounts lists every account. Server admins only.
func (s *Service) ListAccounts(ctx context.Context, p Principal) ([]Account, error) {
	if err := s.requireServerAdmin(ctx, p); err != nil {
		return nil, err
	}
	return collect(ctx, s.db, func(rows *sql.Rows) (Account, error) { return scanAccount(rows) }, `SELECT `+accountColumns+` FROM accounts ORDER BY name COLLATE NOCASE`)
}

// AccountUpdate changes an account; nil fields stay as they are.
type AccountUpdate struct {
	Disabled    *bool
	ServerAdmin *bool
}

// UpdateAccount disables, enables, promotes or demotes an account. Server
// admins only. The last enabled server admin cannot be disabled or demoted.
func (s *Service) UpdateAccount(ctx context.Context, p Principal, name string, u AccountUpdate) (Account, error) {
	if err := s.requireServerAdmin(ctx, p); err != nil {
		return Account{}, err
	}
	if err := s.limitWrite(p); err != nil {
		return Account{}, err
	}
	var a Account
	err := s.writeAs(ctx, p, "", ActionAdmin, func(tx *wtx) error {
		var err error
		a, err = s.accountByName(ctx, tx, name)
		if err != nil {
			return err
		}
		wasAdmin := a.ServerAdmin && !a.Disabled
		if u.Disabled != nil {
			var at sql.NullInt64
			if *u.Disabled {
				at = sql.NullInt64{Int64: ms(s.now()), Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET disabled_at = ? WHERE id = ?`, at, a.ID); err != nil {
				return err
			}
			a.Disabled = *u.Disabled
		}
		if u.ServerAdmin != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET server_admin = ? WHERE id = ?`, *u.ServerAdmin, a.ID); err != nil {
				return err
			}
			a.ServerAdmin = *u.ServerAdmin
		}
		if !wasAdmin || (a.ServerAdmin && !a.Disabled) {
			return nil
		}
		var admins int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts
			WHERE server_admin = 1 AND disabled_at IS NULL`).Scan(&admins); err != nil {
			return err
		}
		if admins == 0 {
			return errorf(CodeLastAdmin, "%s is the last enabled server admin; promote another account first", a.Name)
		}
		return nil
	})
	return a, err
}

// IssueToken issues a token for an account: its own, or any for a server
// admin. ttl 0 means the token does not expire.
func (s *Service) IssueToken(ctx context.Context, p Principal, account, label string, ttl time.Duration) (IssuedToken, error) {
	a, err := s.accountByName(ctx, s.db, account)
	if err != nil {
		return IssuedToken{}, s.hideAccount(ctx, p, err)
	}
	if err := s.requireSelfOrAdmin(ctx, p, a); err != nil {
		return IssuedToken{}, err
	}
	if err := s.limitWrite(p); err != nil {
		return IssuedToken{}, err
	}
	var t IssuedToken
	err = s.writeSelfOrAdmin(ctx, p, a.ID, func(tx *wtx) error {
		var err error
		t, err = s.insertToken(ctx, tx, a, label, ttl)
		return err
	})
	return t, err
}

// hideAccount turns account_not_found into the principal's authorization
// error for callers who could not see the account anyway.
func (s *Service) hideAccount(ctx context.Context, p Principal, err error) error {
	if CodeOf(err) == CodeAccountNotFound {
		if aerr := s.requireServerAdmin(ctx, p); aerr != nil {
			return aerr
		}
	}
	return err
}

const tokenColumns = `t.id, a.name, t.label, t.created_at, t.last_used_at, t.expires_at, t.revoked_at`

func scanToken(rows *sql.Rows) (Token, error) {
	var t Token
	var created int64
	var used, expires, revoked sql.NullInt64
	err := rows.Scan(&t.ID, &t.Account, &t.Label, &created, &used, &expires, &revoked)
	t.CreatedAt = fromMS(created)
	t.LastUsedAt, t.ExpiresAt, t.RevokedAt = fromNullMS(used), fromNullMS(expires), fromNullMS(revoked)
	return t, err
}

// ListTokens lists an account's tokens, without secrets.
func (s *Service) ListTokens(ctx context.Context, p Principal, account string) ([]Token, error) {
	a, err := s.accountByName(ctx, s.db, account)
	if err != nil {
		return nil, s.hideAccount(ctx, p, err)
	}
	if err := s.requireSelfOrAdmin(ctx, p, a); err != nil {
		return nil, err
	}
	return collect(ctx, s.db, scanToken, `SELECT `+tokenColumns+` FROM tokens t JOIN accounts a ON a.id = t.account_id
		WHERE t.account_id = ? ORDER BY t.id`, a.ID)
}

// RevokeToken revokes token id of account, and with it the browser
// sessions it started. It returns the token, or none if it was already
// revoked. The account itself or a server admin may revoke it. A token of
// another account is token_not_found. Without force, revoking the token the
// request is using is own_credential.
func (s *Service) RevokeToken(ctx context.Context, p Principal, account string, id int64, force bool) ([]Token, error) {
	a, err := s.accountByName(ctx, s.db, account)
	if err != nil {
		return nil, s.hideAccount(ctx, p, err)
	}
	if err := s.requireSelfOrAdmin(ctx, p, a); err != nil {
		return nil, err
	}
	if err := s.limitWrite(p); err != nil {
		return nil, err
	}
	var ts []Token
	err = s.writeSelfOrAdmin(ctx, p, a.ID, func(tx *wtx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tokens WHERE account_id = ? AND id = ?`,
			a.ID, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errorf(CodeTokenNotFound, "token %d of %s not found", id, a.Name)
		}
		var err error
		ts, err = s.revokeTokens(ctx, tx, p, `t.account_id = ? AND t.id = ?`, []any{a.ID, id}, force)
		return err
	})
	return ts, err
}

// RevokeAllTokens revokes every token of an account, and with them its
// browser sessions, and returns the tokens it revoked. Without force, an
// account revoking its own tokens with one of them is own_credential.
func (s *Service) RevokeAllTokens(ctx context.Context, p Principal, account string, force bool) ([]Token, error) {
	a, err := s.accountByName(ctx, s.db, account)
	if err != nil {
		return nil, s.hideAccount(ctx, p, err)
	}
	if err := s.requireSelfOrAdmin(ctx, p, a); err != nil {
		return nil, err
	}
	if err := s.limitWrite(p); err != nil {
		return nil, err
	}
	var ts []Token
	err = s.writeSelfOrAdmin(ctx, p, a.ID, func(tx *wtx) error {
		var err error
		ts, err = s.revokeTokens(ctx, tx, p, `t.account_id = ?`, []any{a.ID}, force)
		return err
	})
	return ts, err
}

// revokeTokens revokes the unrevoked tokens matching where and returns
// them. Unless force, it refuses if they include the token p authenticated
// with, directly or through a browser session.
func (s *Service) revokeTokens(ctx context.Context, tx *wtx, p Principal, where string, args []any, force bool) ([]Token, error) {
	query := `SELECT ` + tokenColumns + ` FROM tokens t JOIN accounts a ON a.id = t.account_id
		WHERE ` + where + ` AND t.revoked_at IS NULL ORDER BY t.id`
	ts, err := collect(ctx, tx, scanToken, query, args...)
	if err != nil {
		return nil, err
	}
	if !force {
		own, err := s.credentialToken(ctx, tx, p)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			if t.ID == own {
				return nil, errorf(CodeOwnCredential, "token %d is the one this request uses; pass force to revoke it", t.ID)
			}
		}
	}
	now := s.now()
	for i := range ts {
		if _, err := tx.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE id = ?`, ms(now), ts[i].ID); err != nil {
			return nil, err
		}
		at := fromMS(ms(now))
		ts[i].RevokedAt = &at
	}
	return ts, nil
}

// credentialToken returns the id of the token p authenticated with, or 0.
func (s *Service) credentialToken(ctx context.Context, q querier, p Principal) (int64, error) {
	switch p.Credential.Kind {
	case CredentialToken:
		return p.Credential.ID, nil
	case CredentialSession:
		var id sql.NullInt64
		err := q.QueryRowContext(ctx, `SELECT source_token_id FROM browser_sessions WHERE id = ?`, p.Credential.ID).Scan(&id)
		if isNoRows(err) {
			return 0, nil
		}
		return id.Int64, err
	}
	return 0, nil
}

// SetMembership gives an account a role in a namespace, replacing any it
// had. Namespace admins and server admins only.
func (s *Service) SetMembership(ctx context.Context, p Principal, ns, account string, role Role) (Membership, error) {
	if err := s.authorize(ctx, p, ns, ActionAdmin); err != nil {
		return Membership{}, err
	}
	if err := s.limitWrite(p); err != nil {
		return Membership{}, err
	}
	if err := checkRole(role); err != nil {
		return Membership{}, err
	}
	var m Membership
	err := s.writeAs(ctx, p, ns, ActionAdmin, func(tx *wtx) error {
		nsID, err := namespaceID(ctx, tx, ns)
		if err != nil {
			return err
		}
		a, err := s.accountByName(ctx, tx, account)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (namespace_id, account_id, role) VALUES (?, ?, ?)
			ON CONFLICT (namespace_id, account_id) DO UPDATE SET role = excluded.role`, nsID, a.ID, role); err != nil {
			return err
		}
		m = Membership{Namespace: ns, Account: a.Name, Role: role}
		return nil
	})
	return m, err
}

// RemoveMembership removes an account's role in a namespace. Removing one
// that does not exist is not an error.
func (s *Service) RemoveMembership(ctx context.Context, p Principal, ns, account string) error {
	if err := s.authorize(ctx, p, ns, ActionAdmin); err != nil {
		return err
	}
	if err := s.limitWrite(p); err != nil {
		return err
	}
	return s.writeAs(ctx, p, ns, ActionAdmin, func(tx *wtx) error {
		nsID, err := namespaceID(ctx, tx, ns)
		if err != nil {
			return err
		}
		a, err := s.accountByName(ctx, tx, account)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM memberships WHERE namespace_id = ? AND account_id = ?`, nsID, a.ID)
		return err
	})
}

// ListMemberships lists a namespace's members.
func (s *Service) ListMemberships(ctx context.Context, p Principal, ns string) ([]Membership, error) {
	if err := s.authorize(ctx, p, ns, ActionAdmin); err != nil {
		return nil, err
	}
	nsID, err := namespaceID(ctx, s.db, ns)
	if err != nil {
		return nil, err
	}
	return collect(ctx, s.db, func(rows *sql.Rows) (Membership, error) {
		m := Membership{Namespace: ns}
		err := rows.Scan(&m.Account, &m.Role)
		return m, err
	}, `SELECT a.name, m.role FROM memberships m JOIN accounts a ON a.id = m.account_id
		WHERE m.namespace_id = ? ORDER BY a.name COLLATE NOCASE`, nsID)
}

// Bootstrap creates the first account, a server admin, and its first token.
// It refuses if any account exists. It is for `smoot db bootstrap`,
// which runs against the database directly.
func (s *Service) Bootstrap(ctx context.Context, name string) (IssuedToken, error) {
	var t IssuedToken
	err := s.write(ctx, func(tx *wtx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errorf(CodeAccountExists, "accounts already exist; use smoot admin with a server admin's token, or smoot db recover")
		}
		a, err := s.insertAccount(ctx, tx, name, true)
		if err != nil {
			return err
		}
		t, err = s.insertToken(ctx, tx, a, "bootstrap", 0)
		return err
	})
	return t, err
}

// Recover re-enables an account, makes it a server admin and issues it a
// new token. It is for `smoot db recover`, which runs against the
// database directly, for when no usable server admin token is left.
func (s *Service) Recover(ctx context.Context, name string) (IssuedToken, error) {
	var t IssuedToken
	err := s.write(ctx, func(tx *wtx) error {
		a, err := s.accountByName(ctx, tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET server_admin = 1, disabled_at = NULL WHERE id = ?`, a.ID); err != nil {
			return err
		}
		a.ServerAdmin, a.Disabled = true, false
		t, err = s.insertToken(ctx, tx, a, "recover", 0)
		return err
	})
	return t, err
}

// authorizeRole implements the Roles policy.
func (s *Service) authorizeRole(ctx context.Context, q querier, p Principal, ns string, a Action) error {
	if p.IsAnonymous() {
		return unauthorized("sign in with a token")
	}
	if err := s.checkCredential(ctx, q, p); err != nil {
		return err
	}
	var admin bool
	var role sql.NullString
	err := q.QueryRowContext(ctx, `SELECT a.server_admin, m.role FROM accounts a
		LEFT JOIN memberships m ON m.account_id = a.id
			AND m.namespace_id = (SELECT id FROM namespaces WHERE name = ?)
		WHERE a.id = ?`, ns, p.AccountID).Scan(&admin, &role)
	if err != nil {
		return err
	}
	switch {
	case admin:
		return nil
	case ns == "":
		return errorf(CodeForbidden, "only a server admin can do that")
	case role.Valid && Role(role.String).allows(a):
		return nil
	case role.Valid:
		return errorf(CodeForbidden, "your role in namespace %q (%s) does not allow %s", ns, role.String, a)
	default:
		return errorf(CodeForbidden, "you are not a member of namespace %q", ns)
	}
}
