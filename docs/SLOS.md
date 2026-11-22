# Serving SLOs & Benchmarks

Target latency, throughput, and resource budgets for a 10,000–100,000 asset library over LAN or Tailnet.

## Per-Endpoint Latency Budgets

| Operation | p50 Target | p95 Budget | p99 Target | Implementation Guarantee |
|---|---|---|---|---|
| `GET /capabilities` | <= 3 ms | **<= 15 ms** | <= 25 ms | In-memory struct; zero disk I/O |
| `GET /assets/count` | <= 8 ms | **<= 30 ms** | <= 50 ms | Covering SQLite indexes |
| `GET /assets/range?limit=200&fields=grid` | <= 20 ms | **<= 60 ms** | <= 100 ms | Composite indexes + compact grid projection |
| `POST /ops` (1–20 ops) | <= 15 ms | **<= 50 ms** | <= 90 ms | Dedicated write lock isolated from thumbnail/index workers; SQLite WAL + atomic `.edits` |
| `GET /assets/{id}/blob?tier=t1` (warm) | <= 8 ms | **<= 25 ms** | <= 45 ms | App-data disk cache + `ETag` + `304 Not Modified` |
| `GET /assets/{id}/blob?tier=t1` (cold RAW) | <= 45 ms | **<= 120 ms** | <= 200 ms | Embedded JPEG offset scan (TIFF/EXIF/CR3/RAF); never full RAW decode |
| `GET /assets/{id}/blob?tier=t1` (cold raster) | <= 80 ms | **<= 250 ms** | <= 400 ms | Bounded worker pool (`min(4, NumCPU)`) + decode semaphore |
| `GET /assets/{id}/blob?tier=original` | TTFB <= 15 ms | **TTFB <= 40 ms** | TTFB <= 80 ms | Direct `http.ServeContent` streaming, `206` Range, immediate cancel on client disconnect |
| Startup -> `/capabilities` ready | <= 500 ms | **<= 2000 ms** | <= 3000 ms | Serves immediately from persisted SQLite index; incremental rescan runs in background |

## Resource & Overload Budgets

- **Peak RSS:** <= 256 MiB under 3 concurrent browser clients inside a 512 MiB Docker container.
- **Backpressure:** Thumbnail generation and heavy streaming use bounded concurrency semaphores. Saturation returns fast `429 Too Many Requests` with `Retry-After` rather than queuing unbounded goroutines or hanging sockets.
- **Client abort cancellation:** Aborting an original blob download cancels reading immediately so open file descriptors and active stream counts return to `0` within milliseconds.

## Reproducible Benchmarks

```bash
go test -bench=. -benchmem ./internal/hlc ./internal/sidecar ./internal/index ./internal/thumb
```

Every HTTP response emits `Server-Timing: app;dur=<ms>` so clients and load harnesses can inspect server-side latency directly against each route's `SLOBudgetMs`.
