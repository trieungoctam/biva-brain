DROP FUNCTION IF EXISTS operations_requeue_expired();
DROP TRIGGER IF EXISTS operations_notify_requeue ON operations;
DROP TRIGGER IF EXISTS operations_notify_insert ON operations;
DROP FUNCTION IF EXISTS operations_notify();
