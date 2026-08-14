-- migrations/0009_drop_panelist_role.down.sql
ALTER TYPE user_role RENAME TO user_role_new;
CREATE TYPE user_role AS ENUM ('candidate', 'panelist', 'judge', 'admin');

ALTER TABLE users
    ALTER COLUMN role DROP DEFAULT,
    ALTER COLUMN role TYPE user_role USING (role::text)::user_role,
    ALTER COLUMN role SET DEFAULT 'candidate';

DROP TYPE user_role_new;
