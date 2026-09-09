-- Drop the bootstrap partitions. Detach-then-drop rather than a bare DROP so a
-- rollback on a live system never blocks on the parent's lock any longer than
-- one partition at a time.
DO $$
DECLARE
    part TEXT;
BEGIN
    FOR part IN
        SELECT c.relname
        FROM pg_inherits i
        JOIN pg_class c ON c.oid = i.inhrelid
        JOIN pg_class p ON p.oid = i.inhparent
        WHERE p.relname = 'slots'
    LOOP
        EXECUTE format('ALTER TABLE slots DETACH PARTITION %I', part);
        EXECUTE format('DROP TABLE %I', part);
    END LOOP;
END;
$$;
