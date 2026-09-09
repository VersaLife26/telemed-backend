DROP TABLE IF EXISTS slots_archive;
DROP FUNCTION IF EXISTS create_slot_partitions(DATE, INT);
DROP FUNCTION IF EXISTS create_slot_partition(DATE);
-- Dropping the parent drops every attached partition with it.
DROP TABLE IF EXISTS slots;
