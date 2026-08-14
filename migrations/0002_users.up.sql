CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    campus_email    TEXT NOT NULL UNIQUE,
    phone           TEXT,
    whatsapp        TEXT,
    role            user_role NOT NULL DEFAULT 'candidate',
    group_label     TEXT,                    -- round 1 shuffle batch label
    round1_result   round_result,
    round2_result   round_result,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_users_role ON users (role);
CREATE INDEX idx_users_round1_result ON users (round1_result);
