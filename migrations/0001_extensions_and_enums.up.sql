CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TYPE user_role AS ENUM ('candidate', 'panelist', 'judge', 'admin');
CREATE TYPE round_result AS ENUM ('advanced', 'eliminated');
CREATE TYPE assignment_status AS ENUM ('confirmed', 'pending_query', 'reassigned');
CREATE TYPE query_status AS ENUM ('pending', 'resolved');
CREATE TYPE resolution_type AS ENUM ('swap', 'reassign');
CREATE TYPE eval_status AS ENUM ('checked_in', 'in_progress', 'completed', 'no_show', 'skipped');
CREATE TYPE attendance_status AS ENUM ('pending', 'present', 'no_show');
CREATE TYPE team_side AS ENUM ('A', 'B');
