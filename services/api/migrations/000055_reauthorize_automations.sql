-- Existing active graphs predate task management authorization. Preserve graph/history,
-- requiring explicit reactivation through the current manager boundary.
UPDATE automations SET status = 'inactive', updated_at = NOW() WHERE status = 'active';
