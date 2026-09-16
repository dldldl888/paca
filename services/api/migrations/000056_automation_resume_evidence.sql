-- A run records the exact graph snapshot and explicit sprint it began with.
-- Zero revisions intentionally leave legacy paused runs non-resumable.
ALTER TABLE automations ADD COLUMN graph_revision BIGINT NOT NULL DEFAULT 1;
ALTER TABLE automation_runs ADD COLUMN graph_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE automation_runs ADD COLUMN sprint_id UUID;

CREATE FUNCTION automation_metadata_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.graph_revision := OLD.graph_revision + 1;
    RETURN NEW;
END;
$$;
CREATE TRIGGER automation_metadata_revision
BEFORE UPDATE OF project_id, name, description, status, created_by, created_at, updated_at, deleted_at
ON automations FOR EACH ROW EXECUTE FUNCTION automation_metadata_revision();

-- The revision update commits atomically with every node/edge mutation,
-- including hard deletion and foreign-key cascades. Scheduling tables are separate.
CREATE FUNCTION automation_graph_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        UPDATE automations SET graph_revision = graph_revision + 1 WHERE id = OLD.automation_id;
    END IF;
    IF TG_OP = 'INSERT' THEN
        UPDATE automations SET graph_revision = graph_revision + 1 WHERE id = NEW.automation_id;
    ELSIF TG_OP = 'UPDATE' AND NEW.automation_id IS DISTINCT FROM OLD.automation_id THEN
        UPDATE automations SET graph_revision = graph_revision + 1 WHERE id = NEW.automation_id;
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER automation_node_revision AFTER INSERT OR UPDATE OR DELETE
ON automation_nodes FOR EACH ROW EXECUTE FUNCTION automation_graph_revision();
CREATE TRIGGER automation_edge_revision AFTER INSERT OR UPDATE OR DELETE
ON automation_edges FOR EACH ROW EXECUTE FUNCTION automation_graph_revision();
