CREATE TYPE property_rating AS ENUM ('bad', 'meh', 'good');

CREATE TABLE round_scoring_properties (
    id          BIGSERIAL PRIMARY KEY,
    round_id    BIGINT NOT NULL REFERENCES rounds(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    position    SMALLINT NOT NULL DEFAULT 0,
    is_active   BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_round_property_name UNIQUE (round_id, name)
);

-- Round 1 / 3 (interview) ratings
CREATE TABLE evaluation_property_ratings (
    id             BIGSERIAL PRIMARY KEY,
    evaluation_id  BIGINT NOT NULL REFERENCES evaluations(id) ON DELETE CASCADE,
    property_id    BIGINT NOT NULL REFERENCES round_scoring_properties(id) ON DELETE CASCADE,
    rating         property_rating NOT NULL,
    rated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_eval_property UNIQUE (evaluation_id, property_id)
);

-- Round 2 (debate) ratings
CREATE TABLE participant_property_ratings (
    id             BIGSERIAL PRIMARY KEY,
    participant_id BIGINT NOT NULL REFERENCES debate_participants(id) ON DELETE CASCADE,
    property_id    BIGINT NOT NULL REFERENCES round_scoring_properties(id) ON DELETE CASCADE,
    rating         property_rating NOT NULL,
    rated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_participant_property UNIQUE (participant_id, property_id)
);
