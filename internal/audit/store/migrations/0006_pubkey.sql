-- A session keeps the public key it signed in with, as an authorized_keys
-- line (type and key, no comment), beside its fingerprint: the key itself
-- is there to compare even after the user removed it, or when only a
-- directory (LDAP) held it. Readers and writers name only the columns a
-- table has (store.CH.known), so a console or gateway that is newer than
-- this migration reads and writes the table all the same until it runs.

ALTER TABLE {db}.sessions
    ADD COLUMN IF NOT EXISTS pubkey String DEFAULT ''
;
