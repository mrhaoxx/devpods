-- What a reservation held (reservations v2): its cards and cores, -1 for
-- the whole machine — rows from before shares are whole-machine ones.
ALTER TABLE {db}.reservations ADD COLUMN IF NOT EXISTS gpu Int32 DEFAULT -1, ADD COLUMN IF NOT EXISTS cpu Float64 DEFAULT -1;
