-- Round 2: attendance + score, one judge per slot (the panelist who claimed it)
CREATE TABLE debate_participants (
    id              BIGSERIAL PRIMARY KEY,
    candidate_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slot_id         BIGINT NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
    team            team_side NOT NULL,
    attendance      attendance_status NOT NULL DEFAULT 'pending',
    score           SMALLINT,
    comments        TEXT,
    submitted_at    TIMESTAMPTZ,

    CONSTRAINT uq_candidate_slot_debate UNIQUE (candidate_id, slot_id)
);

CREATE INDEX idx_debate_participants_slot_id ON debate_participants (slot_id);
