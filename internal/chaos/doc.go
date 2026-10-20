// Package chaos holds the Phase 11 fault-injection tests: a daemon killed
// mid-run, a Docker engine that disappears and comes back, and a database
// locked by another process. The daemon tests need Docker and run with
// -tags integration.
package chaos
