CREATE TYPE chat_plan_mode AS ENUM ('plan');
ALTER TABLE chats ADD COLUMN plan_mode chat_plan_mode;
ALTER TABLE chat_messages ADD COLUMN plan_mode chat_plan_mode;
ALTER TABLE chat_queued_messages ADD COLUMN plan_mode chat_plan_mode;
