# PKBFI Storage Migration

PizzaSQL now requires the PKBFI protocol and PizzaKV's durable `.pkvdb` engine. The old text-delimited `.db` file cannot be opened directly by the new engine.

## Upgrade

1. Stop the PizzaSQL and PizzaKV processes that own the legacy file.
2. Keep an immutable backup of the legacy `.db` file.
3. Build or install the matching new PizzaKV and PizzaSQL binaries.
4. Migrate into a new destination:

```bash
pizzakv -migrate=.db -path=.pkvdb
```

5. Start PizzaKV with the migrated file:

```bash
pizzakv -unix -path=.pkvdb
```

6. Start PizzaSQL against `unix:.pizzakv.sock`, or use `pizzasql -kv -kvflags="-path=.pkvdb"` to let PizzaSQL launch PizzaKV.
7. Run point-read, bounded-scan, write, restart, and application smoke tests before removing the backup.

The migration command exits after writing and verifying the destination. It refuses to overwrite an existing destination and does not modify the source file.

## Compatibility

- Deploy the new PizzaKV and PizzaSQL binaries together. New PizzaSQL does not fall back to the delimiter-based protocol.
- Migration copies the final live key/value state. Legacy journal history is not imported, and imported records begin at the migration baseline LSN.
- Existing PizzaSQL row values remain readable as legacy JSON after migration. New and updated rows use the versioned binary tuple encoding, so an eager row rewrite is unnecessary.
- Schema and catalog values remain JSON because they are cold metadata.
- `Column.DefaultExpr` is an additive schema-catalog field (`default_expr`)
  holding the SQL text of a write-time default. An engine without it ignores the
  field, so rolling back to a pre-`default_expr` binary silently loses write-time
  defaults: nullable columns read as NULL and NOT NULL columns reject the insert.
  Constant defaults in the `default` field are unaffected.
- `Schema.Checks` (CHECK constraints) is an additive, optional JSON field. A
  pre-CHECK engine ignores it on read and drops it on the next schema write, so
  rolling back to such an engine silently stops enforcing CHECK constraints;
  restore the newer engine (or re-create the constraints) after a rollback.
- `Schema.PrimaryKeyColumns` (`primary_key_columns`) is an additive, optional
  JSON field for composite primary keys. A single-column catalog still carries
  only `primary_key`; a composite catalog persists `primary_key` as an empty
  string plus the per-column `primary_key` flags and the ordered
  `primary_key_columns` list. A pre-composite engine ignores the new field, so
  rolling back to one collapses key derivation: reads, duplicate detection, and
  updates then address the row by at most one column (or by the hidden rowid),
  which can return the wrong row or report spurious duplicates. Restore the
  newer engine before serving traffic from such a tenant. No row migration is
  needed moving forward: rows written by this engine keep their keys, and
  single-column key bytes are unchanged for older engines.
- Successful PKBFI writes are acknowledged only after PizzaKV's durability sync completes.
- A `.pkvdb` file must have one PizzaSQL process owner. Optimistic scan and
  predicate conflict detection uses process-local generations and is not safe
  when multiple PizzaSQL processes share one storage file.
- Read-only transactions validate scan and indexed-predicate generations, but
  point reads are not revalidated unless the transaction also writes.

## Rollback

Stop the new processes and restart the old binaries against the untouched legacy `.db` backup. Writes accepted into `.pkvdb` after cutover are not copied back to the legacy file, so reconcile or discard them before rollback.

## Production Sequence

For each tenant, migrate and validate independently. Do not migrate a file while its owner process is running, and do not replace a live tenant's files as part of a benchmark or endurance experiment.
