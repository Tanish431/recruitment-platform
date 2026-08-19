ALTER TABLE slots ADD COLUMN scorer_judge_id BIGINT REFERENCES users(id);
ALTER TABLE slots ADD COLUMN motion TEXT;
ALTER TABLE evaluations ADD COLUMN motion TEXT;
ALTER TABLE candidate_unavailability ADD COLUMN reason TEXT;
