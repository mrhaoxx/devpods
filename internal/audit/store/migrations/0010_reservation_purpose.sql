-- A reservation's purpose (what it is for, as its holder says), kept in its
-- history: the reservation object goes 90 days after it ends; the history
-- stays, so what each was for stays with it.
ALTER TABLE {db}.reservations ADD COLUMN IF NOT EXISTS purpose String DEFAULT '';
