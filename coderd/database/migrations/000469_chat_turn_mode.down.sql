ALTER TABLE chat_queued_messages DROP COLUMN plan_mode;
ALTER TABLE chat_messages DROP COLUMN plan_mode;
ALTER TABLE chats DROP COLUMN plan_mode;
DROP TYPE chat_plan_mode;
