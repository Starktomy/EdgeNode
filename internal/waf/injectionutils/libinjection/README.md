# Maintained GoEdge libinjection snapshot

The vendor base identifies itself as libinjection 3.9.1 and contains GoEdge's
strict-mode parameter and customized detection lists. `3.9.1-goedge-security1`
and GoEdge revision `26` identify the local reviewed security backports; they
are not an assertion that the entire library was replaced by upstream 4.0.1.

The following published upstream fixes were backported without replacing the
GoEdge strict-mode interface:

- [PR 60](https://github.com/libinjection/libinjection/pull/60): tokenize an
  incomplete numeric exponent without treating it as a SQL word.
- [PR 65](https://github.com/libinjection/libinjection/pull/65): return an error
  for invalid HTML parser state instead of terminating the process. The local
  integer ABI retains `0` (clean), `1` (detected), `-1` (parser error); the Go
  wrapper rejects every nonzero result. Unrelated SQL parser assertions remain
  unchanged.
- [PR 70](https://github.com/libinjection/libinjection/pull/70) and
  [PR 72](https://github.com/libinjection/libinjection/pull/72): bound event-name
  comparisons and validate complete entity prefixes before reading them.
- [PR 93](https://github.com/libinjection/libinjection/pull/93), released in
  4.0.1: distinguish a high byte from the HTML parser's EOF sentinel.

These changes follow the maintenance project's
[release history](https://github.com/libinjection/libinjection/releases).
PR 62 only changes the upstream test driver and is not a production-parser
patch. Preserve `COPYING` with source and binary distributions.

Normal short/empty/boundary and strict-mode regressions:

```sh
go test ./internal/waf/injectionutils -run 'Test(SecurityNative|DetectSQLInjection|DetectXSS)'
```

The native boundary unit test compiles a small local C test with the existing
C compiler. It uses local constant inputs and performs no network requests.
