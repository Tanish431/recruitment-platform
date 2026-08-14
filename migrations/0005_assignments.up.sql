CREATE TABLE assignments (
    id              BIGSERIAL PRIMARY KEY,
    candidate_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slot_id         BIGINT NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
    status          assignment_status NOT NULL DEFAULT 'confirmed',
    team            team_side,              -- round 2 only, null for round 1/3
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- one active assignment per candidate per slot
    CONSTRAINT uq_candidate_slot UNIQUE (candidate_id, slot_id)
);

CREATE INDEX idx_assignments_candidate_id ON assignments (candidate_id);
CREATE INDEX idx_assignments_slot_id ON assignments (slot_id);
CREATE INDEX idx_assignments_status ON assignments (status);
