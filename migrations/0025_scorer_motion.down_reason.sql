ALTER TABLE candidate_unavailability DROP COLUMN IF EXISTS reason;
ALTER TABLE evaluations DROP COLUMN IF EXISTS motion;
ALTER TABLE slots DROP COLUMN IF EXISTS motion;
ALTER TABLE slots DROP COLUMN IF EXISTS scorer_judge_id;
