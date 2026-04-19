The MIT License (MIT)

Copyright (c) 2026 - present, Yasushi Itoh

Portions of this project are derived from upstream
[PocketBase](https://github.com/pocketbase/pocketbase) and retain their
original copyright notices at the top of each affected file:

- `form_data.go`, `form_data_test.go` — taken verbatim from
  `github.com/pocketbase/pocketbase/plugins/jsvm` with only the package
  name changed, Copyright (c) 2022 - present, Gani Georgiev. MIT License.
- `mapper.go`, `mapper_test.go` — upstream code with the goja-specific
  FieldMapper struct removed; same upstream copyright applies.
- `jsvm.go`, `binds.go`, `binds_test.go` — substantially rewritten for
  the Ramune engine and Workers-style handlers, but started as ports
  of the upstream files; same upstream copyright applies.
- `pool.go` — Soda-specific channel-based rewrite, but retains the
  public API shape (vmsPool, newPool, run) from upstream's pool.go;
  upstream-shape copyright applies alongside the rewrite copyright.
- `internal/types/generated/types.d.ts` — the bulk is generated from
  PocketBase's Go types via tygoja; same upstream copyright applies.

The Workers-style additions inside `types.d.ts` (Env, D1Database,
KVNamespace, ExecutionContext, ScheduledEvent, WorkersHandler, and
related interfaces) and every other file in this repository are
original to Soda.

Permission is hereby granted, free of charge, to any person obtaining a copy of this software
and associated documentation files (the "Software"), to deal in the Software without restriction,
including without limitation the rights to use, copy, modify, merge, publish, distribute,
sublicense, and/or sell copies of the Software, and to permit persons to whom the Software
is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or
substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING
BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM,
DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
