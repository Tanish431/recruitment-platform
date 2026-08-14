CREATE TABLE queries (
    id                          BIGSERIAL PRIMARY KEY,
    assignment_id               BIGINT NOT NULL REFERENCES assignments(id) ON DELETE CASCADE,
    reason                      TEXT NOT NULL,
    status                      query_status NOT NULL DEFAULT 'pending',
    resolution_type             resolution_type,
    swapped_with_assignment_id  BIGINT REFERENCES assignments(id),
    resolved_by                 BIGINT REFERENCES users(id),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at                 TIMESTAMPTZ
);

CREATE INDEX idx_queries_status ON queries (status);
CREATE INDEX idx_queries_assignment_id ON queries (assignment_id);
