// Package resourcelease is the client SDK for the NimoOS core resource
// scheduler (/v1/resources/leases). It owns the lease wire protocol, the
// acquire state machine (queued long-poll, retry, fail-open), heartbeats,
// release, the lease registry closed on shutdown and the cross-process
// handoff file for resident leases.
//
// The SDK never invents policy: Priority is always given by the caller, the
// budget map is passed through untouched and the business decides what to do
// with a fail-open or rejected Result.
package resourcelease
