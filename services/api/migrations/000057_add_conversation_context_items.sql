-- Existing histories cannot be certified by reconstructing natural-language prompts.
ALTER TABLE agent_conversations ADD COLUMN context_items JSONB;
ALTER TABLE agent_conversations ADD CONSTRAINT conversation_context_array CHECK (context_items IS NULL OR jsonb_typeof(context_items) = 'array');
