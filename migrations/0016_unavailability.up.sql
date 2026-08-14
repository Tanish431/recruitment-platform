CREATE TABLE candidate_unavailability (
    id              BIGSERIAL PRIMARY KEY,
    candidate_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    round_id        BIGINT NOT NULL REFERENCES rounds(id) ON DELETE CASCADE,
    unavailable_dates DATE[] NOT NULL,
    note            TEXT,
    submitted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT uq_candidate_round_unavailability UNIQUE (candidate_id, round_id)
);
