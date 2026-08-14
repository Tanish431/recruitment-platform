ALTER TABLE locations ADD CONSTRAINT uq_location_round_name UNIQUE (round_id, name);
