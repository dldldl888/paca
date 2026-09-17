CREATE TABLE task_drafts (
 id UUID PRIMARY KEY,
 project_id UUID NOT NULL REFERENCES projects(id),
 owner_user_id UUID NOT NULL REFERENCES users(id),
 version INTEGER NOT NULL CHECK(version>0),
 state TEXT NOT NULL CHECK(state IN ('editable','published')),
 content JSONB NOT NULL,
 publication_id UUID UNIQUE,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX task_drafts_owner_project ON task_drafts(owner_user_id, project_id, updated_at DESC);
ALTER TABLE notifications ADD COLUMN source_event_id TEXT;
CREATE UNIQUE INDEX notifications_source_recipient ON notifications(source_event_id,recipient_user_id) WHERE source_event_id IS NOT NULL;
