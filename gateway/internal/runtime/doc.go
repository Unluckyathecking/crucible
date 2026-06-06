// Package runtime assembles config-driven, default-off resilience and tracing
// providers into a Components value ready for injection into proxy.New and
// server.Deps. Call Assemble with a validated *config.Config; with all knobs
// at their defaults every component is a safe zero value — no exporter is
// dialled and no retry/breaker logic is installed.
//
// Requires Go 1.20 or later (errors.Join).
package runtime
