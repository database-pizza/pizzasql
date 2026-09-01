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
- Successful PKBFI writes are acknowledged only after PizzaKV's durability sync completes.

## Rollback

Stop the new processes and restart the old binaries against the untouched legacy `.db` backup. Writes accepted into `.pkvdb` after cutover are not copied back to the legacy file, so reconcile or discard them before rollback.

## Production Sequence

For each tenant, migrate and validate independently. Do not migrate a file while its owner process is running, and do not replace a live tenant's files as part of a benchmark or endurance experiment.
