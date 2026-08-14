CREATE TABLE judge_stations (
    judge_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    location_id BIGINT NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
    round_id    BIGINT NOT NULL REFERENCES rounds(id) ON DELETE CASCADE,
    PRIMARY KEY (judge_id, round_id)  -- one station per judge per round
);
