-- Queue Go ⇄ Python (M0, S0.2.1 + S0.2.3).
-- NOTIFY 'biva_operations' đánh thức ai-worker khi có job mới hoặc job được trả lại hàng đợi; worker vẫn poll định kỳ
-- nên NOTIFY bị mất (mất kết nối LISTEN) chỉ làm chậm, không làm sót job.

CREATE FUNCTION operations_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('biva_operations', NEW.kind);
    RETURN NULL;
END;
$$;

CREATE TRIGGER operations_notify_insert
    AFTER INSERT ON operations
    FOR EACH ROW WHEN (NEW.status = 'queued')
    EXECUTE FUNCTION operations_notify();

CREATE TRIGGER operations_notify_requeue
    AFTER UPDATE OF status ON operations
    FOR EACH ROW WHEN (NEW.status = 'queued' AND OLD.status IS DISTINCT FROM 'queued')
    EXECUTE FUNCTION operations_notify();

-- Trả job hết lease (worker chết / treo) về hàng đợi; hết lượt thử thì chuyển failed.
-- Scheduler (leader) gọi định kỳ. Một nguồn sự thật cho cả Go và test Python.
CREATE FUNCTION operations_requeue_expired() RETURNS INTEGER
LANGUAGE sql AS $$
    WITH expired AS (
        UPDATE operations
        SET status      = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'queued' END,
            error       = 'lease expired (worker ' || COALESCE(locked_by, '?') || ')',
            locked_by   = NULL,
            lease_until = NULL,
            run_after   = now(),
            updated_at  = now()
        WHERE status = 'running' AND lease_until < now()
        RETURNING 1
    )
    SELECT count(*)::int FROM expired;
$$;
