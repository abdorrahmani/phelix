package grpc

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"google.golang.org/protobuf/proto"
)

// healthResultCache makes mutating health commands idempotent by request_id,
// mirroring the rollback/matrix/webhook ledgers' begin→replay semantics but
// deliberately IN-MEMORY, not durable on disk. Health commands are synchronous
// request/response on the AgentStream (the result is returned on the same stream
// immediately), unlike those async MonitorStream commands whose durable ledger
// exists to replay a result on a later reconnect; and a health edit is
// transactional (ConfigManager.Mutate's atomic write), so a failed command never
// half-applied and is safe to re-execute. So the cache stores only SUCCESSFUL
// results: a redelivery of the same (request_id, payload) replays the recorded
// success instead of re-applying; a request_id reused for a DIFFERENT payload is
// an IDEMPOTENCY_CONFLICT; a failed command is not cached and a same-id retry
// re-executes. The dedup window is the most recent `max` request_ids (FIFO).
// Cross-restart redelivery (cache lost) is covered by the handlers' own natural
// idempotency (add/remove/update converge on the same state). See
// docs/health-backend-contract.md.
type healthResultCache struct {
	mu      sync.Mutex
	max     int
	entries map[string]healthCacheEntry
	order   []string
}

type healthCacheEntry struct {
	fingerprint string
	result      *pb.HealthResult
}

const healthResultCacheCapacity = 256

var healthResults = newHealthResultCache(healthResultCacheCapacity)

func newHealthResultCache(max int) *healthResultCache {
	return &healthResultCache{max: max, entries: make(map[string]healthCacheEntry)}
}

// lookup reports what to do with an incoming mutating command:
//   - (true, result, nil): a redelivery of an identical prior command — replay result
//   - (false, nil, nil):   not seen (or no request_id) — execute it, then store
//   - (false, nil, err):   request_id reused for a different payload (IDEMPOTENCY_CONFLICT)
func (c *healthResultCache) lookup(cmd *pb.HealthCommand) (bool, *pb.HealthResult, error) {
	id := cmd.GetRequestId()
	if id == "" {
		return false, nil, nil // no key to dedup on; execute without caching
	}
	fp, err := healthCommandFingerprint(cmd)
	if err != nil {
		return false, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok {
		return false, nil, nil
	}
	if e.fingerprint != fp {
		return false, nil, phelixerr.New(phelixerr.CodeIdempotencyConflict,
			"request_id already used for a different health command payload")
	}
	return true, cloneHealthResult(e.result), nil
}

// store records a successful command's result for replay. No-op for an empty
// request_id or a non-success result (a failed, transactional health edit is
// safe to re-execute on retry, so it is intentionally not cached).
func (c *healthResultCache) store(cmd *pb.HealthCommand, result *pb.HealthResult) {
	id := cmd.GetRequestId()
	if id == "" || result == nil || !result.GetSuccess() {
		return
	}
	fp, err := healthCommandFingerprint(cmd)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[id]; !ok {
		if len(c.order) >= c.max {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, oldest)
		}
		c.order = append(c.order, id)
	}
	c.entries[id] = healthCacheEntry{fingerprint: fp, result: cloneHealthResult(result)}
}

// healthCommandFingerprint hashes a command's payload with the transport
// identity fields (request_id, server_id) excluded, so the same logical command
// fingerprints identically regardless of which stream connection delivered it.
func healthCommandFingerprint(cmd *pb.HealthCommand) (string, error) {
	clone := proto.Clone(cmd).(*pb.HealthCommand)
	clone.RequestId = ""
	clone.ServerId = ""
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeUnknown, "fingerprint health command", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// cloneHealthResult deep-copies a cached result so a replay never aliases the
// stored message (proto.Clone is required for the embedded message state).
func cloneHealthResult(r *pb.HealthResult) *pb.HealthResult {
	if r == nil {
		return nil
	}
	return proto.Clone(r).(*pb.HealthResult)
}
