/*
  Provides a mapping from job_name to job_id with descending order
  for time.

  - Read most recent job id for same job name.
*/

CREATE TABLE IF NOT EXISTS job_name_to_id (
  job_name text,
  job_id uuid,
  update_time timeuuid,
  PRIMARY KEY (job_name, update_time)
) WITH CLUSTERING ORDER BY (update_time DESC)
  AND bloom_filter_fp_chance = 0.1
  AND caching = {'keys': 'ALL', 'rows_per_partition': 'NONE'}
  AND comment = ''
  AND compaction = {'class': 'org.apache.cassandra.db.compaction.LeveledCompactionStrategy', 'sstable_size_in_mb': '64', 'unchecked_tombstone_compaction': 'true'}
  AND compression = {'chunk_length_in_kb': '64', 'class': 'org.apache.cassandra.io.compress.LZ4Compressor'}
  AND crc_check_chance = 1.0
  AND dclocal_read_repair_chance = 0.1
  AND gc_grace_seconds = 864000
  AND max_index_interval = 2048
  AND memtable_flush_period_in_ms = 0
  AND min_index_interval = 128
  AND read_repair_chance = 0.0;
