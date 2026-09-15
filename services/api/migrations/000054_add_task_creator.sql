-- Historical tasks have no trustworthy creator; never infer it from reporter_id.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS creator_user_id UUID NULL REFERENCES users(id);
