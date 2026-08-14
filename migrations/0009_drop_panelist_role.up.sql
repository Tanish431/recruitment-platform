-- migrations/0009_drop_panelist_role.up.sql
ALTER TYPE user_role RENAME TO user_role_old;
CREATE TYPE user_role AS ENUM ('candidate', 'judge', 'admin');

ALTER TABLE users
    ALTER COLUMN role DROP DEFAULT,
    ALTER COLUMN role TYPE user_role USING (
        CASE role::text
            WHEN 'panelist' THEN 'judge'
            ELSE role::text
        END
    )::user_role,
    ALTER COLUMN role SET DEFAULT 'candidate';

DROP TYPE user_role_old;
