ALTER TABLE project_drafts ADD COLUMN IF NOT EXISTS publication_receipt JSONB;
ALTER TABLE project_drafts DROP CONSTRAINT IF EXISTS project_drafts_state_check;
ALTER TABLE project_drafts ADD CONSTRAINT project_drafts_state_check CHECK (state IN ('editable', 'published'));
