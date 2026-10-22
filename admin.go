package main

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (srv *server) handleAdminUsers(w http.ResponseWriter, r *http.Request, admin *user) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	// strpos instead of ILIKE so %/_ in q are literal.
	rows, err := srv.db.Query(ctx, `SELECT id,email,coalesce(name,''),credits,is_admin,disabled,created_at,last_login_at
		FROM users WHERE strpos(lower(email),lower($1))>0 OR strpos(lower(coalesce(name,'')),lower($1))>0
		ORDER BY id LIMIT $2`, q, limitParam(r, 50))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var email, name string
		var credits int
		var isAdmin, disabled bool
		var created time.Time
		var last *time.Time
		if err := rows.Scan(&id, &email, &name, &credits, &isAdmin, &disabled, &created, &last); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		out = append(out, map[string]any{"id": id, "email": email, "name": name, "credits": credits,
			"isAdmin": isAdmin, "disabled": disabled, "createdAt": created, "lastLoginAt": last})
	}
	srv.logger.Info("admin action", "admin", admin.Email, "action", "list_users", "q", q)
	writeJSON(w, http.StatusOK, out)
}

func (srv *server) handleAdminCredits(w http.ResponseWriter, r *http.Request, admin *user) {
	var body struct {
		Email  string `json:"email"`
		Amount int    `json:"amount"`
		Note   string `json:"note"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	email, note := strings.ToLower(strings.TrimSpace(body.Email)), strings.TrimSpace(body.Note)
	if email == "" || note == "" || body.Amount == 0 || body.Amount > 10000 || body.Amount < -10000 {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	tx, err := srv.db.Begin(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer tx.Rollback(ctx)

	var uid int64
	var balance int
	err = tx.QueryRow(ctx, `UPDATE users SET credits = credits + $2 WHERE email=$1 AND credits + $2 >= 0 RETURNING id, credits`, email, body.Amount).Scan(&uid, &balance)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE email=$1)`, email).Scan(&exists)
		if exists {
			writeErr(w, http.StatusConflict, "would_go_negative")
		} else {
			writeErr(w, http.StatusNotFound, "user_not_found")
		}
		return
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO credit_transactions(user_id,amount,balance_after,kind,admin_user_id,note) VALUES ($1,$2,$3,'admin_adjust',$4,$5)`,
			uid, body.Amount, balance, admin.ID, note)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		srv.logger.Error("admin credits failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	srv.logger.Info("admin action", "admin", admin.Email, "action", "adjust_credits", "target", email, "amount", body.Amount, "note", note, "balance", balance)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": email, "credits": balance})
}

func (srv *server) handleAdminDisable(w http.ResponseWriter, r *http.Request, admin *user) {
	var body struct {
		Email    string `json:"email"`
		Disabled bool   `json:"disabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if email == admin.Email {
		writeErr(w, http.StatusBadRequest, "cannot_disable_self")
		return
	}
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	tag, err := srv.db.Exec(ctx, `UPDATE users SET disabled=$2 WHERE email=$1`, email, body.Disabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, http.StatusNotFound, "user_not_found")
		return
	}
	srv.logger.Info("admin action", "admin", admin.Email, "action", "set_disabled", "target", email, "disabled", body.Disabled)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": email, "disabled": body.Disabled})
}

func (srv *server) handleAdminTransactions(w http.ResponseWriter, r *http.Request, admin *user) {
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	rows, err := srv.db.Query(ctx, `SELECT t.id,u.email,t.amount,t.balance_after,t.kind,t.order_id,a.email,coalesce(t.note,''),t.created_at
		FROM credit_transactions t JOIN users u ON u.id=t.user_id LEFT JOIN users a ON a.id=t.admin_user_id
		ORDER BY t.created_at DESC, t.id DESC LIMIT $1`, limitParam(r, 100))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var email, kind, note string
		var amount, balance int
		var orderID *int64
		var adminEmail *string
		var created time.Time
		if err := rows.Scan(&id, &email, &amount, &balance, &kind, &orderID, &adminEmail, &note, &created); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		out = append(out, map[string]any{"id": id, "email": email, "amount": amount, "balanceAfter": balance,
			"kind": kind, "orderId": orderID, "adminEmail": adminEmail, "note": note, "createdAt": created})
	}
	srv.logger.Info("admin action", "admin", admin.Email, "action", "list_transactions")
	writeJSON(w, http.StatusOK, out)
}

func (srv *server) handleAdminOrders(w http.ResponseWriter, r *http.Request, admin *user) {
	status := r.URL.Query().Get("status")
	if status != "" && status != "pending" && status != "completed" && status != "refunded" {
		writeErr(w, http.StatusBadRequest, "invalid_status")
		return
	}
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	rows, err := srv.db.Query(ctx, `SELECT o.id,u.email,o.drink,o.price,o.status,coalesce(o.refund_reason,''),o.created_at,o.completed_at
		FROM orders o JOIN users u ON u.id=o.user_id WHERE ($1='' OR o.status=$1)
		ORDER BY o.created_at DESC, o.id DESC LIMIT $2`, status, limitParam(r, 100))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var email, drink, st, reason string
		var price int
		var created time.Time
		var completed *time.Time
		if err := rows.Scan(&id, &email, &drink, &price, &st, &reason, &created, &completed); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		out = append(out, map[string]any{"id": id, "email": email, "drink": drink, "price": price, "status": st,
			"refundReason": reason, "createdAt": created, "completedAt": completed})
	}
	srv.logger.Info("admin action", "admin", admin.Email, "action", "list_orders", "status", status)
	writeJSON(w, http.StatusOK, out)
}
