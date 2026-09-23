ALTER TABLE project_drafts ADD COLUMN IF NOT EXISTS publication_id UUID;
ALTER TABLE project_drafts ADD COLUMN IF NOT EXISTS published_project_id UUID REFERENCES projects(id);
ALTER TABLE project_drafts ADD COLUMN IF NOT EXISTS publication_receipt JSONB;
ALTER TABLE project_drafts DROP CONSTRAINT IF EXISTS project_drafts_state_check;
ALTER TABLE project_drafts ADD CONSTRAINT project_drafts_state_check CHECK (state IN ('editable','published'));
CREATE UNIQUE INDEX IF NOT EXISTS project_drafts_publication_id_key ON project_drafts(publication_id) WHERE publication_id IS NOT NULL;
