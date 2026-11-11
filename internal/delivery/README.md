# Durable delivery boundaries

The spool stores private, bounded batches for retry after an API outage or Node
restart. Access-log persistence and network delivery use independent workers;
slow RPCs do not prevent later logs from reaching disk. Access logs use a 128 MiB
spool and a 64 MiB RAM admission budget, with each emitted batch capped at 4 MiB.
New arrivals exceeding the memory/entry limits increase `Dropped()`.
The local `accesslog` console uses an independent 128-frame/1 MiB best-effort
text queue and per-connection write deadlines. Slow console frames increase
`ViewerDropped()`, never block disk publication and never count as lost spool
logs.

`Spool.Append` returns an empty ID only when publication did not occur. A
nonempty ID with an error means the file was renamed into place but durability
confirmation failed. Retain that identity; restoring and republishing the same
data under a new ID can duplicate statistics. Successful publication with no
error syncs the file and parent directory. Ack syncs the directory after unlink.

Disk-full errors retain admitted, unpublished logs in bounded RAM and keep their
byte charge. They do not increment `Dropped()`. A recovered sender can drain the
spool while those logs wait for disk space. Permanent malformed/validation
failures move to `quarantine/` with a reason sidecar; network errors retain the
same batch for retry with backoff. Quarantine has separate entry/byte limits.
When that budget is exhausted, the original batch remains and the error requires
operator attention. Inspect and repair quarantined data before replay; preserve
the original identity for statistics.

Quit and termination callbacks call `FlushToDisk(ctx)` with a five-second bound.
It asks the persistence worker to publish RAM logs without waiting for RPC. A
full disk or timeout is reported explicitly; no successful flush is claimed.
Request/reply deadlines also bound shutdown when the worker is unavailable or a
filesystem call stalls. A timed-out write may still finish afterward.

Sudden process death, power loss, or an unsuccessful shutdown flush can lose RAM
data that was never durably published. Access-log delivery is **at least once**:
the stable `x-edge-access-log-batch` metadata currently has no API receipt-based
deduplication, so a lost server ACK can duplicate log rows. Statistics use their
separate transactional receipt protocol; do not describe these guarantees as
exactly-once access logging.

Local failure-path checks:

```sh
go test -tags=community -race ./internal/delivery
go test -tags=community -race ./internal/nodes -run 'Test(AccessLog|PermanentAccessLog)'
```
