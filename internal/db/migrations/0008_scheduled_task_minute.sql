-- 0008_scheduled_task_minute - add minute-of-hour to scheduled tasks.
--
-- 0007 supported whole-hour scheduling only (at_hour), so "7:30am" could not be
-- expressed and was silently rounded to 7:00. Add at_minute (0-59) so a task fires at
-- at_hour:at_minute local. DEFAULT 0 keeps every existing row firing at HH:00 exactly as
-- before, so this is a non-breaking addition.
--
-- Pure DDL: connection pragmas are applied per-connection in db.go.

ALTER TABLE scheduled_tasks
    ADD COLUMN at_minute INTEGER NOT NULL DEFAULT 0; -- minute of at_hour, 0-59
