ALTER TABLE debate_participants DROP COLUMN IF EXISTS final_notes;
ALTER TABLE debate_participants DROP COLUMN IF EXISTS speaker_notes;
ALTER TABLE evaluations DROP COLUMN IF EXISTS co_judge_name;
ALTER TABLE evaluations DROP COLUMN IF EXISTS final_notes;
ALTER TABLE evaluations DROP COLUMN IF EXISTS speaker_notes;
