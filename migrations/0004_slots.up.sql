CREATE TABLE slots (
    id              BIGSERIAL PRIMARY KEY,
    round_id        BIGINT NOT NULL REFERENCES rounds(id) ON DELETE CASCADE,
    location_id     BIGINT NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
    start_time      TIMESTAMPTZ NOT NULL,
    duration_min    SMALLINT NOT NULL DEFAULT 15,
    capacity        SMALLINT NOT NULL DEFAULT 1,
    filled_count    SMALLINT NOT NULL DEFAULT 0,
    created_by      BIGINT REFERENCES users(id),   -- panelist who claimed it (round 2 only)
    claimed_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT chk_filled_within_capacity CHECK (filled_count <= capacity),
    -- Enforces FCFS at the DB level: two panelists can't claim the same
    -- (location, time) inside the same round.
    CONSTRAINT uq_slot_location_time UNIQUE (round_id, location_id, start_time)
);

CREATE INDEX idx_slots_round_location_time ON slots (round_id, location_id, start_time);
