# Changelog

## v0.1.1

- A rejected API key (HTTP 401/403) stops sending for an hour with one warning, instead of every batch going to a dead key; sending resumes, with a notice, once the key is accepted again.

## v0.1.0

- First release: `zharp.Init` and `Debug`/`Info`/`Warn`/`Error`/`Fatal`, from `Config` or `ZHARP_*` environment variables.
- Errors carry their type and a stack (where logged, or their own via `%+v`).
- `NewSlogHandler` for `log/slog`.
- `Recover` and `Middleware` for panics.
- Background sending, batches, retries; `Close` and `Flush` send what is left.
