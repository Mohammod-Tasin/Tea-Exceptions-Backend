CREATE TABLE users (
    id            bigserial PRIMARY KEY,
    google_sub    text NOT NULL UNIQUE,
    email         text NOT NULL UNIQUE,
    name          text,
    picture_url   text,
    is_admin      boolean NOT NULL DEFAULT false,
    credits       integer NOT NULL DEFAULT 0 CHECK (credits >= 0),
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

CREATE TABLE sessions (
    token_hash   bytea PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    user_agent   text,
    ip           text
);
CREATE INDEX sessions_user_id_idx ON sessions(user_id);
CREATE INDEX sessions_expires_at_idx ON sessions(expires_at);

CREATE TABLE orders (
    id            bigserial PRIMARY KEY,
    user_id       bigint NOT NULL REFERENCES users(id),
    drink         text NOT NULL CHECK (drink IN ('tea','coffee','hotwater')),
    price         integer NOT NULL CHECK (price >= 0),
    status        text NOT NULL CHECK (status IN ('pending','completed','refunded')),
    refund_reason text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    completed_at  timestamptz
);
CREATE INDEX orders_status_created_idx ON orders(status, created_at);

-- Append-only ledger: rows are only ever inserted.
CREATE TABLE credit_transactions (
    id            bigserial PRIMARY KEY,
    user_id       bigint NOT NULL REFERENCES users(id),
    amount        integer NOT NULL CHECK (amount <> 0),
    balance_after integer NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('recharge','purchase','refund','admin_adjust')),
    order_id      bigint REFERENCES orders(id),
    admin_user_id bigint REFERENCES users(id),
    note          text,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX credit_transactions_user_idx ON credit_transactions(user_id, created_at DESC);

-- Placeholder for the future payment gateway.
CREATE TABLE payments (
    id           bigserial PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES users(id),
    credits      integer NOT NULL,
    amount_bdt   integer NOT NULL,
    provider     text,
    provider_ref text UNIQUE,
    status       text NOT NULL CHECK (status IN ('created','paid','failed','cancelled')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    paid_at      timestamptz
);
