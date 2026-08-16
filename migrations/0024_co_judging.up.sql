CREATE TABLE slot_co_judges (
    id                            BIGSERIAL PRIMARY KEY,
    slot_id                       BIGINT NOT NULL REFERENCES slots(id) ON DELETE CASCADE,
    judge_id                      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    host_marked_present           BOOLEAN NOT NULL DEFAULT false,
    co_judge_marked_host_present  BOOLEAN NOT NULL DEFAULT false,
    joined_at                     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_slot_co_judge UNIQUE (slot_id, judge_id)
);

ALTER TABLE slots ADD COLUMN team_a_prep TEXT;
ALTER TABLE slots ADD COLUMN team_b_prep TEXT;
