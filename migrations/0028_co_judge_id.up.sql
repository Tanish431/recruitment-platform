ALTER TABLE evaluations ADD COLUMN co_judge_id BIGINT REFERENCES users(id);
-- co_judge_name (free text) stays for backward compatibility with anything
-- already submitted, but new submissions use co_judge_id going forward.
