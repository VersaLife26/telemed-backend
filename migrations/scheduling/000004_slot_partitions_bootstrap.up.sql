-- Pre-create twelve months of slot partitions starting with the current month.
--
-- The maintenance job (cron, monthly) calls create_slot_partitions() again to
-- keep a rolling twelve-month runway, so this migration only has to get the
-- system off the ground. Both paths are idempotent.
SELECT create_slot_partitions(CURRENT_DATE, 12);

-- Safety net. Without a DEFAULT partition an INSERT for a start_at outside
-- every declared range fails outright -- which, on the daily generation job,
-- means *no doctor gets slots* rather than one doctor getting none. The
-- default catches it, the runbook checks it is empty, and the monthly job is
-- what keeps it that way.
--
-- Trade-off: while a DEFAULT partition exists, attaching a new partition takes
-- an ACCESS EXCLUSIVE lock on it and scans it to prove no row belongs in the
-- new range. That scan is instantaneous on an empty table, which is exactly why
-- the runbook alerts on it being non-empty.
DO $$
BEGIN
    IF to_regclass('svc_scheduling.slots_default') IS NULL THEN
        EXECUTE 'CREATE TABLE slots_default PARTITION OF slots DEFAULT';
    END IF;
END;
$$;
