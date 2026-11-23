package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/claude"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestMigrationPreservesIdentityUnknownFieldsAndTTL(t *testing.T) {
	s := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ctx := context.Background()
	raw := `{"ClientID":"stable","UserAgent":"claude-cli/2.1.283 (external, cli)","UpdatedAt":123,"StainlessOS":"Windows","future":{"large":9007199254740993}}`
	if err := rdb.Set(ctx, "fingerprint:7", raw, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	p := claude.CompatibilityProfile{Lang: "js", PackageVersion: "0.112.1", OS: "Linux", Arch: "x64", Runtime: "node", RuntimeVersion: "v22.19.0"}
	if _, err := migrate(ctx, rdb, 7, p, false); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get("fingerprint:7")
	if before != raw {
		t.Fatal("preview changed cache")
	}
	if _, err := migrate(ctx, rdb, 7, p, true); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get("fingerprint:7")
	var previous, updated map[string]json.RawMessage
	json.Unmarshal([]byte(raw), &previous)
	json.Unmarshal([]byte(after), &updated)
	for _, field := range []string{"ClientID", "UserAgent", "UpdatedAt", "future"} {
		if string(previous[field]) != string(updated[field]) {
			t.Fatalf("changed %s", field)
		}
	}
	if string(updated["StainlessOS"]) != `"Linux"` {
		t.Fatal("profile was not updated")
	}
	if s.TTL("fingerprint:7") != time.Hour {
		t.Fatal("TTL changed")
	}
	if err := compareAndSet(ctx, rdb, "fingerprint:7", raw, `{}`); err == nil {
		t.Fatal("overwrote concurrent change")
	}
	if _, err := migrate(ctx, rdb, 8, p, true); err == nil {
		t.Fatal("created missing fingerprint")
	}
	for _, invalid := range []string{`null`, `{}`, `{"ClientID":"stable","UserAgent":4}`} {
		rdb.Set(ctx, "fingerprint:9", invalid, time.Hour)
		if _, err := migrate(ctx, rdb, 9, p, true); err == nil {
			t.Fatal("accepted invalid cached identity")
		}
	}
	rdb.Persist(ctx, "fingerprint:7")
	if _, err := migrate(ctx, rdb, 7, p, true); err == nil {
		t.Fatal("accepted nonexpiring fingerprint")
	}
}

func TestParseAccountIDs(t *testing.T) {
	ids, err := parseAccountIDs("1, 2,1")
	if err != nil || len(ids) != 2 {
		t.Fatal(ids, err)
	}
	for _, raw := range []string{"", "0", "-1", "1,", "abc"} {
		if _, err := parseAccountIDs(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}
