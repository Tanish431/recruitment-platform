-- Round 1
INSERT INTO rounds (number, name) VALUES (1, 'Round 1 - Interviews')
    ON CONFLICT (number) DO NOTHING;

INSERT INTO locations (round_id, name)
SELECT id, 'Lib' FROM rounds WHERE number = 1
    ON CONFLICT DO NOTHING;

-- Round 2
INSERT INTO rounds (number, name) VALUES (2, 'Round 2 - Debates')
    ON CONFLICT (number) DO NOTHING;

INSERT INTO locations (round_id, name)
SELECT id, loc.name
FROM rounds, (VALUES ('Rotunda'), ('FD-1'), ('FD-2'), ('Location D'), ('Location E')) AS loc(name)
WHERE rounds.number = 2
ON CONFLICT DO NOTHING;

-- Round 3
INSERT INTO rounds (number, name) VALUES (3, 'Round 3 - Final')
    ON CONFLICT (number) DO NOTHING;
