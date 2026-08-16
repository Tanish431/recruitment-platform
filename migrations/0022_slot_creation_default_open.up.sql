ALTER TABLE rounds ALTER COLUMN slot_creation_open SET DEFAULT true;
UPDATE rounds SET slot_creation_open = true;
