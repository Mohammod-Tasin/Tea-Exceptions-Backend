package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/api/idtoken"
)

const sessionCookieName = "tea_session"

type user struct {
	ID       int64
	Email    string
	Name     string
	Picture  string
	Credits  int
	IsAdmin  bool
	Disabled bool
}

func (u *user) json() map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "name": u.Name, "picture": u.Picture, "credits": u.Credits, "isAdmin": u.IsAdmin}
}

type googleIdentity struct {
	Sub, Email, Name, Picture string
}

// verifyGoogleToken validates a Google ID token for our client id.
func verifyGoogleToken(clientID string) func(context.Context, string) (*googleIdentity, error) {
	return func(ctx context.Context, credential string) (*googleIdentity, error) {
		p, err := idtoken.Validate(ctx, credential, clientID)
		if err != nil {
			return nil, err
		}
		if p.Issuer != "accounts.google.com" && p.Issuer != "https://accounts.google.com" {
			return nil, errors.New("bad issuer")
		}
		verified, _ := p.Claims["email_verified"].(bool)
		email, _ := p.Claims["email"].(string)
		if !verified || email == "" {
			return nil, errors.New("email not verified")
		}
		name, _ := p.Claims["name"].(string)
		pic, _ := p.Claims["picture"].(string)
		return &googleIdentity{Sub: p.Subject, Email: strings.ToLower(email), Name: name, Picture: pic}, nil
	}
}

func hashToken(raw string) []byte {
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sessionCookie builds the session cookie; days <= 0 expires it.
func sessionCookie(token string, days int) *http.Cookie {
	c := &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		MaxAge: days * 24 * 3600,
	}
	if days <= 0 {
		c.MaxAge = -1
	}
	return c
}

func (srv *server) createSession(ctx context.Context, userID int64, ua, ip string) (string, error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	_, err = srv.db.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at,user_agent,ip)
		VALUES ($1,$2,now()+make_interval(days => $3),$4,$5)`, hashToken(tok), userID, srv.cfg.SessionDays, ua, ip)
	return tok, err
}

func (srv *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"googleClientId":   srv.cfg.GoogleClientID,
		"prices":           srv.cfg.Prices,
		"rechargePackages": srv.cfg.Packages,
	})
}

func (srv *server) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Credential string `json:"credential"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Credential == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	vctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	id, err := srv.verifyGoogle(vctx, body.Credential)
	if err != nil {
		srv.logger.Info("google login rejected", "error", err.Error())
		writeErr(w, http.StatusUnauthorized, "invalid_credential")
		return
	}

	ctx, cancel2 := dbCtx(r.Context())
	defer cancel2()
	u := &user{}
	err = srv.db.QueryRow(ctx, `INSERT INTO users(google_sub,email,name,picture_url,is_admin,last_login_at)
		VALUES ($1,$2,$3,$4,$5,now())
		ON CONFLICT (google_sub) DO UPDATE SET email=EXCLUDED.email, name=EXCLUDED.name,
			picture_url=EXCLUDED.picture_url, is_admin=EXCLUDED.is_admin, last_login_at=now()
		RETURNING id,email,coalesce(name,''),coalesce(picture_url,''),credits,is_admin,disabled`,
		id.Sub, id.Email, id.Name, id.Picture, srv.cfg.AdminEmails[id.Email],
	).Scan(&u.ID, &u.Email, &u.Name, &u.Picture, &u.Credits, &u.IsAdmin, &u.Disabled)
	if err != nil {
		srv.logger.Error("login upsert failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if u.Disabled {
		writeErr(w, http.StatusForbidden, "disabled")
		return
	}
	tok, err := srv.createSession(r.Context(), u.ID, r.UserAgent(), clientIP(r))
	if err != nil {
		srv.logger.Error("create session failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	http.SetCookie(w, sessionCookie(tok, srv.cfg.SessionDays))
	srv.logger.Info("login", "user", u.Email)
	writeJSON(w, http.StatusOK, u.json())
}

func (srv *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		ctx, cancel := dbCtx(r.Context())
		defer cancel()
		if _, err := srv.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash=$1`, hashToken(c.Value)); err != nil {
			srv.logger.Error("logout failed", "error", err)
		}
	}
	http.SetCookie(w, sessionCookie("", 0))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (srv *server) handleMe(w http.ResponseWriter, r *http.Request, u *user) {
	writeJSON(w, http.StatusOK, u.json())
}

// auth wraps a handler that needs a logged-in (and not disabled) user.
func (srv *server) auth(next func(http.ResponseWriter, *http.Request, *user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil || c.Value == "" {
			writeErr(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		ctx, cancel := dbCtx(r.Context())
		defer cancel()
		h := hashToken(c.Value)
		u := &user{}
		var lastSeen time.Time
		err = srv.db.QueryRow(ctx, `SELECT u.id,u.email,coalesce(u.name,''),coalesce(u.picture_url,''),u.credits,u.is_admin,u.disabled,s.last_seen_at
			FROM sessions s JOIN users u ON u.id=s.user_id
			WHERE s.token_hash=$1 AND s.expires_at>now()`, h,
		).Scan(&u.ID, &u.Email, &u.Name, &u.Picture, &u.Credits, &u.IsAdmin, &u.Disabled, &lastSeen)
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if err != nil {
			srv.logger.Error("session lookup failed", "error", err)
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		if u.Disabled {
			writeErr(w, http.StatusForbidden, "disabled")
			return
		}
		if time.Since(lastSeen) > 5*time.Minute {
			srv.db.Exec(ctx, `UPDATE sessions SET last_seen_at=now() WHERE token_hash=$1`, h)
		}
		next(w, r, u)
	}
}

func (srv *server) adminOnly(next func(http.ResponseWriter, *http.Request, *user)) http.HandlerFunc {
	return srv.adminLimit.wrap(srv.auth(func(w http.ResponseWriter, r *http.Request, u *user) {
		if !u.IsAdmin {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		next(w, r, u)
	}))
}

func (srv *server) deleteExpiredSessions(ctx context.Context) {
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	if _, err := srv.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`); err != nil {
		srv.logger.Error("session cleanup failed", "error", err)
	}
}
