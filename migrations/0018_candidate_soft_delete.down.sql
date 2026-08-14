DROP INDEX IF EXISTS idx_users_role_active;

ALTER TABLE users DROP COLUMN IF EXISTS is_active;
