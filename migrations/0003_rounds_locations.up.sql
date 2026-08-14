CREATE TABLE rounds (
    id          BIGSERIAL PRIMARY KEY,
    number      SMALLINT NOT NULL UNIQUE,   -- 1, 2, 3
    name        TEXT NOT NULL,
    config      JSONB NOT NULL DEFAULT '{}',
    slot_creation_open BOOLEAN NOT NULL DEFAULT false, -- used by round 2
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE locations (
    id          BIGSERIAL PRIMARY KEY,
    round_id    BIGINT NOT NULL REFERENCES rounds(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_locations_round_id ON locations (round_id);
