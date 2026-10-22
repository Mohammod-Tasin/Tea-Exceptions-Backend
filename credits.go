package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// handleUnlock charges the user and queues the drink, all-or-nothing:
// reserve machine -> DB tx (debit + order + ledger) -> queue command.
func (srv *server) handleUnlock(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		Drink string `json:"drink"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	price, ok := srv.cfg.Prices[body.Drink]
	if !ok || !validDrinks[body.Drink] {
		srv.logger.Info("unlock request", "drink", body.Drink, "result", "bad_request")
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid drink"})
		return
	}

	now := time.Now()
	switch srv.state.reserve(now) {
	case "offline":
		srv.logger.Info("unlock request", "drink", body.Drink, "result", "offline")
		writeJSON(w, http.StatusConflict, map[string]any{"error": "machine offline"})
		return
	case "not_ready":
		srv.state.mu.Lock()
		ms := srv.state.machineStateLocked(now)
		srv.state.mu.Unlock()
		srv.logger.Info("unlock request", "drink", body.Drink, "result", "not_ready", "machineState", ms)
		writeJSON(w, http.StatusConflict, map[string]any{"error": "not_ready", "machineState": ms})
		return
	case "busy":
		srv.logger.Info("unlock request", "drink", body.Drink, "result", "busy")
		writeJSON(w, http.StatusConflict, map[string]any{"error": "machine busy"})
		return
	}

	orderID, credits, err := srv.purchase(r.Context(), u.ID, body.Drink, price)
	if err != nil {
		srv.state.release()
		if errors.Is(err, errInsufficient) {
			srv.logger.Info("unlock request", "user", u.Email, "drink", body.Drink, "result", "insufficient_credits")
			writeJSON(w, http.StatusPaymentRequired, map[string]any{"error": "insufficient_credits", "credits": credits, "price": price})
			return
		}
		srv.logger.Error("purchase failed", "user", u.Email, "error", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}

	srv.state.queue(now, command{ID: orderID, Drink: body.Drink, Seconds: unlockSeconds})
	srv.logger.Info("unlock request", "user", u.Email, "drink", body.Drink, "order", orderID, "result", "ok")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "orderId": orderID, "drink": body.Drink, "seconds": unlockSeconds, "credits": credits})
}

var errInsufficient = errors.New("insufficient credits")

// purchase debits the user, creates a pending order and (if price > 0) the
// ledger row in one transaction. On errInsufficient, credits is the current balance.
func (srv *server) purchase(ctx context.Context, userID int64, drink string, price int) (orderID int64, credits int, err error) {
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	tx, err := srv.db.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `UPDATE users SET credits = credits - $2 WHERE id=$1 AND credits >= $2 RETURNING credits`, userID, price).Scan(&credits)
	if errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		srv.db.QueryRow(ctx, `SELECT credits FROM users WHERE id=$1`, userID).Scan(&credits)
		return 0, credits, errInsufficient
	}
	if err != nil {
		return 0, 0, err
	}
	if err = tx.QueryRow(ctx, `INSERT INTO orders(user_id,drink,price,status) VALUES ($1,$2,$3,'pending') RETURNING id`, userID, drink, price).Scan(&orderID); err != nil {
		return 0, 0, err
	}
	if price > 0 {
		if _, err = tx.Exec(ctx, `INSERT INTO credit_transactions(user_id,amount,balance_after,kind,order_id) VALUES ($1,$2,$3,'purchase',$4)`,
			userID, -price, credits, orderID); err != nil {
			return 0, 0, err
		}
	}
	return orderID, credits, tx.Commit(ctx)
}

// completeOrder marks a pending order completed. Reports whether it acted.
func (srv *server) completeOrder(ctx context.Context, id int64) (bool, error) {
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	tag, err := srv.db.Exec(ctx, `UPDATE orders SET status='completed', completed_at=now() WHERE id=$1 AND status='pending'`, id)
	return tag.RowsAffected() == 1, err
}

// refundOrder refunds a pending order exactly once: the status flip is the
// guard, and the credit + ledger row ride in the same transaction.
func (srv *server) refundOrder(ctx context.Context, id int64, reason string) (bool, error) {
	ctx, cancel := dbCtx(ctx)
	defer cancel()
	tx, err := srv.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var userID int64
	var price int
	err = tx.QueryRow(ctx, `UPDATE orders SET status='refunded', refund_reason=$2 WHERE id=$1 AND status='pending' RETURNING user_id, price`, id, reason).Scan(&userID, &price)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if price > 0 {
		var balance int
		if err = tx.QueryRow(ctx, `UPDATE users SET credits = credits + $2 WHERE id=$1 RETURNING credits`, userID, price).Scan(&balance); err != nil {
			return false, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO credit_transactions(user_id,amount,balance_after,kind,order_id,note) VALUES ($1,$2,$3,'refund',$4,$5)`,
			userID, price, balance, id, reason); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	srv.logger.Info("order refunded", "order", id, "reason", reason, "price", price)
	return true, nil
}

// resolveTimeout settles an order the device never confirmed.
func (srv *server) resolveTimeout(ctx context.Context, id int64) {
	var err error
	if srv.cfg.RefundOnTimeout {
		_, err = srv.refundOrder(ctx, id, "timeout")
	} else {
		_, err = srv.completeOrder(ctx, id)
	}
	if err != nil {
		srv.logger.Error("resolve order failed", "order", id, "error", err)
	}
}

// armTimeout settles the order if the device neither consumed nor rejected it
// by the end of the busy window.
func (srv *server) armTimeout(cmd command) {
	time.AfterFunc(time.Duration(cmd.Seconds)*time.Second+2*time.Second, func() {
		srv.resolveTimeout(context.Background(), cmd.ID)
	})
}

// sweepPending settles pending orders older than UNLOCK_SECONDS+60s. Runs at
// startup (crash safety) and periodically (commands that were never collected).
func (srv *server) sweepPending(ctx context.Context) {
	qctx, cancel := dbCtx(ctx)
	defer cancel()
	rows, err := srv.db.Query(qctx, `SELECT id FROM orders WHERE status='pending' AND created_at < now() - make_interval(secs => $1)`, unlockSeconds+60)
	if err != nil {
		srv.logger.Error("sweep failed", "error", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		srv.resolveTimeout(ctx, id)
	}
}

func limitParam(r *http.Request, def int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, 100)
}

func (srv *server) handleMyOrders(w http.ResponseWriter, r *http.Request, u *user) {
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	rows, err := srv.db.Query(ctx, `SELECT id,drink,price,status,coalesce(refund_reason,''),created_at,completed_at
		FROM orders WHERE user_id=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, u.ID, limitParam(r, 20))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var drink, status, reason string
		var price int
		var created time.Time
		var completed *time.Time
		if err := rows.Scan(&id, &drink, &price, &status, &reason, &created, &completed); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		out = append(out, map[string]any{"id": id, "drink": drink, "price": price, "status": status,
			"refundReason": reason, "createdAt": created, "completedAt": completed})
	}
	writeJSON(w, http.StatusOK, out)
}

func (srv *server) handleMyTransactions(w http.ResponseWriter, r *http.Request, u *user) {
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	rows, err := srv.db.Query(ctx, `SELECT id,amount,balance_after,kind,order_id,coalesce(note,''),created_at
		FROM credit_transactions WHERE user_id=$1 ORDER BY created_at DESC, id DESC LIMIT $2`, u.ID, limitParam(r, 50))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var amount, balance int
		var kind, note string
		var orderID *int64
		var created time.Time
		if err := rows.Scan(&id, &amount, &balance, &kind, &orderID, &note, &created); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
		out = append(out, map[string]any{"id": id, "amount": amount, "balanceAfter": balance, "kind": kind,
			"orderId": orderID, "note": note, "createdAt": created})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRecharge is a placeholder until the payment gateway exists.
func (srv *server) handleRecharge(w http.ResponseWriter, r *http.Request, u *user) {
	var body struct {
		Credits int `json:"credits"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	valid := false
	for _, p := range srv.cfg.Packages {
		valid = valid || p.Credits == body.Credits
	}
	if !valid {
		writeErr(w, http.StatusBadRequest, "invalid_package")
		return
	}
	// TODO(payments): insert a `payments` row (status 'created'), call the
	// gateway to create a checkout session, return its URL. Credits must only
	// be granted by the gateway's verified webhook (kind 'recharge'), never here.
	writeErr(w, http.StatusNotImplemented, "payment_not_available")
}
