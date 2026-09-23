CREATE TABLE project_drafts (
 id UUID PRIMARY KEY,
 owner_user_id UUID NOT NULL REFERENCES users(id),
 chat_session_id UUID NOT NULL REFERENCES agent_chat_sessions(id),
 version INTEGER NOT NULL CHECK(version > 0),
 state TEXT NOT NULL CHECK(state IN ('editable')),
 content JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX project_drafts_owner_updated ON project_drafts(owner_user_id, updated_at DESC, id DESC);
CREATE INDEX project_drafts_session ON project_drafts(chat_session_id);
