# teamachine

Go backend (stdlib `net/http` + pgx + Google idtoken) for a tea/coffee vending
machine. The website signs users in with Google, spends credits via
`POST /api/unlock`; an ESP32 long-polls `GET /api/device/poll` to get the
command. State of the machine is in memory; users, credits and orders are in
PostgreSQL.

## Env vars

| Var | Default | Notes |
|-----|---------|-------|
| `DATABASE_URL` | - | **required**, e.g. `postgres://user:pw@tea-db:5432/teamachine?sslmode=disable` |
| `GOOGLE_CLIENT_ID` | - | **required**, OAuth client id used to validate ID tokens |
| `ADMIN_EMAILS` | empty | comma separated; `is_admin` is recomputed on every login |
| `PRICE_TEA` / `PRICE_COFFEE` / `PRICE_HOTWATER` | `5` / `10` / `0` | credits (1 credit = 1 BDT) |
| `FRONTEND_ORIGIN` | `*` | CORS origin **and** the Origin required by the CSRF check - set it in production |
| `REFUND_ON_TIMEOUT` | `true` | refund when the device never confirms a drink |
| `SESSION_DAYS` | `30` | session / cookie lifetime |
| `RECHARGE_PACKAGES` | `50,100,200` | credit amounts; price in BDT equals credits |
| `PORT` | `8080` | |
| `UNLOCK_SECONDS` | `25` | busy window after a command |
| `DEVICE_KEY` | empty | if set, device must send `X-Device-Key` on `/api/device/*` |
| `STATUS_STALE_SECONDS` | `30` | older device status makes `machineState` `unknown` |
| `TEMP_TABLE` | `4.10:78,4.50:88,4.53:89` | `volts:celsius` pairs for `tempC` |

Startup retries the DB for 60s (then exits non-zero) and applies
`migrations/*.sql` in order (tracked in `schema_migrations`, one transaction
each, under an advisory lock).

## Endpoints

Auth is the `tea_session` cookie (HttpOnly, Secure, SameSite=Lax). Every
non-GET `/api/*` request except `/api/device/*` must send
`Origin: $FRONTEND_ORIGIN` and `Content-Type: application/json` (else 403).
Rate limits are per client IP (first `X-Forwarded-For`): auth 10/min, unlock
20/min, admin 60/min (429).

Public
- `GET /healthz`
- `GET /api/config` - `googleClientId`, `prices`, `rechargePackages`
- `GET /api/status` - machine status (no personal data)

Session
- `POST /api/auth/google` `{"credential": "<Google ID token>"}` - logs in, sets cookie, returns user
- `POST /api/auth/logout`
- `GET /api/me` - `{id,email,name,picture,credits,isAdmin}` or 401
- `GET /api/me/orders?limit=20`, `GET /api/me/transactions?limit=50` (newest first, max 100)
- `POST /api/unlock` `{"drink":"tea|coffee|hotwater"}` - 200 `{ok,orderId,drink,seconds,credits}`;
  401 not logged in; 402 `insufficient_credits` (`credits`, `price`); 409 offline / not_ready / busy
- `POST /api/recharge/checkout` `{"credits":n}` - validates the package, then **501** `payment_not_available`
  (gateway not wired yet; nothing is credited)

Admin (`is_admin`, else 403)
- `GET /api/admin/users?q=&limit=50`
- `POST /api/admin/credits` `{"email","amount","note"}` - |amount| <= 10000, note required, 409 if balance would go negative
- `POST /api/admin/users/disable` `{"email","disabled"}` - not yourself
- `GET /api/admin/transactions?limit=100`, `GET /api/admin/orders?status=&limit=100`

Device (`X-Device-Key`): `GET /api/device/poll`, `POST /api/device/consumed`,
`POST /api/device/status`, `POST /api/device/rejected`. The command is
`{id, drink, seconds}` where `id` is the order id.

## Data model

- `users` - google_sub, email (lowercase), credits (CHECK >= 0), is_admin, disabled
- `sessions` - sha256 of the token (raw token never stored), expiry; expired rows purged hourly
- `orders` - drink, price, status `pending|completed|refunded`, refund_reason
- `credit_transactions` - append-only ledger (`recharge|purchase|refund|admin_adjust`), signed
  `amount`, `balance_after`; written in the same transaction as the `users.credits` change
- `payments` - placeholder for the future gateway

## Purchase and refund rules

1. Machine must be online/ready/not busy; it is reserved for the request.
2. One transaction: debit credits (only if enough), create `pending` order, ledger `purchase` row (skipped for price 0).
3. The order id is queued as the device command id.

An order leaves `pending` exactly once (`UPDATE ... WHERE status='pending'`):
- `/api/device/consumed` -> `completed`
- `/api/device/rejected` -> `refunded` (reason from the device)
- busy window ends with no report -> `refunded` with reason `timeout` (or `completed` if `REFUND_ON_TIMEOUT=false`)
- on startup, and every 30s, pending orders older than `UNLOCK_SECONDS+60s` are settled the same way

A refund restores the price and writes a `refund` ledger row in one transaction.

## Granting credits as admin

Log in on the site with an `ADMIN_EMAILS` account, copy the `tea_session`
cookie value from the browser dev tools, then:

```sh
curl -X POST https://cafe.zerotouch.com.co/api/admin/credits \
  -H 'Origin: https://cafe.zerotouch.com.co' \
  -H 'Content-Type: application/json' \
  -b 'tea_session=PASTE_COOKIE_VALUE' \
  -d '{"email":"user@example.com","amount":50,"note":"cash top-up"}'
```

Use a negative `amount` to take credits back.

## Tests

`go test ./...`; with Docker running, `go test -tags integration ./...` also
starts `postgres:16-alpine` and exercises purchase, refund and admin flows.
