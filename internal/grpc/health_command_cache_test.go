package grpc

import (
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func cacheAddCmd(reqID, name, url string) *pb.HealthCommand {
	return &pb.HealthCommand{
		RequestId: reqID,
		Command:   &pb.HealthCommand_Add{Add: &pb.HealthAddCommand{AppId: "app-1", Name: name, Url: url}},
	}
}

func TestHealthResultCache_ReplaySamePayload(t *testing.T) {
	c := newHealthResultCache(8)
	cmd := cacheAddCmd("r1", "api", "http://localhost:3000/health")

	replay, _, err := c.lookup(cmd)
	if err != nil || replay {
		t.Fatalf("first lookup = replay %v, err %v; want miss", replay, err)
	}
	c.store(cmd, &pb.HealthResult{RequestId: "r1", Success: true, Message: "done"})

	replay, cached, err := c.lookup(cacheAddCmd("r1", "api", "http://localhost:3000/health"))
	if err != nil || !replay {
		t.Fatalf("replay lookup = replay %v, err %v; want replay", replay, err)
	}
	if cached.GetMessage() != "done" {
		t.Fatalf("replayed result = %q, want the stored result", cached.GetMessage())
	}
}

func TestHealthResultCache_ConflictOnDifferentPayload(t *testing.T) {
	c := newHealthResultCache(8)
	c.store(cacheAddCmd("r1", "api", "http://a"), &pb.HealthResult{RequestId: "r1", Success: true})

	_, _, err := c.lookup(cacheAddCmd("r1", "api", "http://DIFFERENT"))
	if !phelixerr.IsCode(err, phelixerr.CodeIdempotencyConflict) {
		t.Fatalf("err = %v, want IDEMPOTENCY_CONFLICT", err)
	}
}

func TestHealthResultCache_FailuresNotCached(t *testing.T) {
	c := newHealthResultCache(8)
	cmd := cacheAddCmd("r1", "api", "http://a")
	c.store(cmd, &pb.HealthResult{RequestId: "r1", Success: false, Error: "boom"})

	replay, _, err := c.lookup(cmd)
	if err != nil || replay {
		t.Fatalf("a failed result must not be cached: replay %v err %v", replay, err)
	}
}

func TestHealthResultCache_EmptyRequestIDNeverCaches(t *testing.T) {
	c := newHealthResultCache(8)
	cmd := cacheAddCmd("", "api", "http://a")
	c.store(cmd, &pb.HealthResult{Success: true})
	replay, _, err := c.lookup(cmd)
	if err != nil || replay {
		t.Fatalf("empty request_id must never dedup: replay %v err %v", replay, err)
	}
}

func TestHealthResultCache_CapacityEvictsOldest(t *testing.T) {
	c := newHealthResultCache(2)
	for _, id := range []string{"r1", "r2", "r3"} {
		cmd := cacheAddCmd(id, "api", "http://a")
		c.store(cmd, &pb.HealthResult{RequestId: id, Success: true})
	}
	if replay, _, _ := c.lookup(cacheAddCmd("r1", "api", "http://a")); replay {
		t.Fatal("r1 should have been evicted at capacity 2")
	}
	if replay, _, _ := c.lookup(cacheAddCmd("r3", "api", "http://a")); !replay {
		t.Fatal("r3 should still be cached")
	}
}

func TestHealthCommandFingerprint_ExcludesTransportIDs(t *testing.T) {
	a, err := healthCommandFingerprint(cacheAddCmd("a", "api", "http://x"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := healthCommandFingerprint(cacheAddCmd("b", "api", "http://x"))
	diff, _ := healthCommandFingerprint(cacheAddCmd("a", "api", "http://y"))
	if a != b {
		t.Fatal("request_id must not affect the fingerprint")
	}
	if a == diff {
		t.Fatal("a payload change must change the fingerprint")
	}
}
