ALTER TABLE users ADD COLUMN is_active BOOLEAN NOT NULL DEFAULT true;

CREATE INDEX idx_users_role_active ON users (role, is_active);
