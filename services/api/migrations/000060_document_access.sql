ALTER TABLE documents ADD COLUMN visibility text;
UPDATE documents SET visibility = 'legacy_project';
ALTER TABLE documents ALTER COLUMN visibility SET DEFAULT 'private';
ALTER TABLE documents ALTER COLUMN visibility SET NOT NULL;
ALTER TABLE documents ADD COLUMN owner_user_id uuid REFERENCES users(id);
UPDATE documents d SET owner_user_id = pm.user_id FROM project_members pm
WHERE d.created_by = pm.id AND pm.member_type = 'human';
ALTER TABLE documents ADD COLUMN source_document_id uuid REFERENCES documents(id);
ALTER TABLE documents ADD COLUMN access_version bigint NOT NULL DEFAULT 1;
ALTER TABLE documents ADD CONSTRAINT document_visibility CHECK (visibility IN ('private','project','task','legacy_project'));
ALTER TABLE documents ADD CONSTRAINT document_private_owner CHECK (visibility != 'private' OR owner_user_id IS NOT NULL);
ALTER TABLE documents ADD CONSTRAINT document_source_shell CHECK (source_document_id IS NULL OR (source_document_id != id AND content IS NULL));
CREATE TABLE document_tasks (
 document_id uuid NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
 task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 PRIMARY KEY(document_id, task_id)
);
CREATE INDEX document_tasks_task ON document_tasks(task_id);
CREATE FUNCTION check_document_task_project() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM documents d JOIN tasks t ON t.project_id=d.project_id WHERE d.id=NEW.document_id AND t.id=NEW.task_id AND t.deleted_at IS NULL) THEN
  RAISE EXCEPTION 'document task must belong to the same project';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER document_task_project BEFORE INSERT OR UPDATE ON document_tasks FOR EACH ROW EXECUTE FUNCTION check_document_task_project();
CREATE FUNCTION check_document_source() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND (OLD.source_document_id IS DISTINCT FROM NEW.source_document_id OR OLD.owner_user_id IS DISTINCT FROM NEW.owner_user_id) THEN
  RAISE EXCEPTION 'document owner and source are immutable';
 END IF;
 IF NEW.source_document_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM documents WHERE id=NEW.source_document_id AND source_document_id IS NULL AND deleted_at IS NULL) THEN
  RAISE EXCEPTION 'document source must be a live canonical original';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER document_source BEFORE INSERT OR UPDATE ON documents FOR EACH ROW EXECUTE FUNCTION check_document_source();
