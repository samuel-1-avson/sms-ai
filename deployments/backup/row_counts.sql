-- Row count of every table in the public schema, one "table count" line each
-- (psql -At -F ' '). backup.sh stores it next to each dump, taken in the
-- dump's snapshot; scripts/restore_drill.sh compares a restore against it.
SELECT table_name,
       (xpath('/row/n/text()',
              query_to_xml(format('SELECT count(*) AS n FROM %I.%I', table_schema, table_name),
                           false, true, '')))[1]::text AS row_count
FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
ORDER BY table_name;
