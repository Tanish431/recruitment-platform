-- Round 1: live judging queue, check-in/claim/skip lifecycle
CREATE TABLE evaluations (
    id                  BIGSERIAL PRIMARY KEY,
    candidate_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slot_id             BIGINT NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
    judge_id            BIGINT REFERENCES users(id),
    status              eval_status NOT NULL DEFAULT 'checked_in',
    checked_in_at       TIMESTAMPTZ,
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    score               SMALLINT,
    comments            TEXT,
    duration_seconds    INTEGER,
    skip_count          SMALLINT NOT NULL DEFAULT 0,

    CONSTRAINT uq_candidate_slot_eval UNIQUE (candidate_id, slot_id)
);

CREATE INDEX idx_evaluations_status ON evaluations (status);
CREATE INDEX idx_evaluations_slot_id ON evaluations (slot_id);
CREATE INDEX idx_evaluations_judge_id ON evaluations (judge_id);
