// Package cache provides a private, bounded, single-owner filesystem cache for
// immutable, SHA-256-keyed OCI objects (blobs and manifests).
//
// A caller admits an object with [Store.Begin], streams it into the returned
// [Fill], and publishes it with [Fill.Commit] only after its size and digest
// verify. A partial object is never visible. [Store.Get] returns a pinned
// [Reader] for a committed object, verifying a persisted body once on its
// first read after startup.
// One process owns a cache directory, enforced by a file lock.
//
// See the repository README for caching behavior and docs/deployment.md for
// storage, capacity, and recovery.
package cache
