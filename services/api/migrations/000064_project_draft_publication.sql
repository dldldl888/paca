ALTER TABLE project_drafts ADD COLUMN IF NOT EXISTS publication_id UUID;
ALTER TABLE project_drafts ADD COLUMN IF NOT EXISTS published_project_id UUID REFERENCES projects(id);
CREATE UNIQUE INDEX IF NOT EXISTS project_drafts_publication_id_key ON project_drafts(publication_id) WHERE publication_id IS NOT NULL;
