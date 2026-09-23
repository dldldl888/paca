CREATE TABLE IF NOT EXISTS project_draft_files (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    draft_id UUID NOT NULL REFERENCES project_drafts(id) ON DELETE CASCADE,
    file_id UUID NOT NULL UNIQUE REFERENCES files(id) ON DELETE CASCADE,
    owner_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    processing_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (processing_status IN ('pending','ready','failed','unverified')),
    processing_error TEXT,
    extracted_text TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_project_draft_files_draft_file UNIQUE (draft_id, file_id)
);

CREATE INDEX IF NOT EXISTS idx_project_draft_files_owner_updated
    ON project_draft_files(owner_user_id, updated_at DESC, id DESC);
