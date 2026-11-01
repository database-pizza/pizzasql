# Managed proxy error relay (version 1)

API-key scope validation happens in the managed pgproxy, but PostgreSQL
transaction and extended-protocol recovery state belongs to the engine. A proxy
must not generate its own `ReadyForQuery('I')` after rejecting a statement: the
backend may still have an active transaction or outstanding extended messages.

## Discovery and frame

Supporting engines advertise startup ParameterStatus
`pizzasql.proxy_errors = 1` and `protocol.proxy_error_relay: true` in
`GET /capabilities`. The proxy uses this extension only after discovery.

Replace a rejected frontend frame, in the same stream position, with:

- Message type: `y` (private extension, not a PostgreSQL message).
- Big-endian length: `5` (four length bytes plus one payload byte).
- Payload: `Q` for a rejected simple Query, or `P` for a rejected Parse.

No SQL, parameters, key scopes or credentials are included in this frame. The
engine returns SQLSTATE `42501` with a generic permission-denied message. An
already failed transaction stays failed and returns `25P02`.

For `Q`, the engine sends the error followed by its real ReadyForQuery state.
For `P`, it fails the extended cycle and discards subsequent frontend messages
until Sync, where it sends its real ReadyForQuery state. Flush and Terminate
remain usable during recovery. An error inside a transaction marks it failed;
ROLLBACK or ROLLBACK TO SAVEPOINT recovers it normally.

The frame cannot execute SQL or grant permissions. The engine listener remains
trusted loopback-only; this extension does not add authentication to that
listener. Ordinary clients need not send it.

## Mixed-version rollout

An updated proxy must not send `y` to an engine that did not advertise version
1. It instead fails closed with FATAL `42501` and closes the tunnel. Closing the
backend rolls back any uncommitted transaction. The client must reconnect;
there is no synthetic ReadyForQuery on that fallback path.

Deploying a proxy does not promote tenant engines. Upgrade the approved engine
pair through the release catalog before expecting recoverable scope errors on
that tenant. Engine registration/promotion and app deployment remain separate
operator-approved operations.
