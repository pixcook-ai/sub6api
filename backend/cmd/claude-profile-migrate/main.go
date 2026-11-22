// claude-profile-migrate explicitly updates selected cached SDK profiles.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/claude"
	"github.com/redis/go-redis/v9"
)

const redisEnv = "SUB2API_PROFILE_MIGRATION_REDIS_URL"

var replaceProfile = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
if redis.call('PTTL', KEYS[1]) <= 0 then return -1 end
redis.call('SET', KEYS[1], ARGV[2], 'KEEPTTL')
return 1
`)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("claude-profile-migrate", flag.ContinueOnError)
	file := flags.String("profile-file", "", "complete compatibility profile JSON file")
	ids := flags.String("account-ids", "", "comma-separated positive account IDs")
	apply := flags.Bool("apply", false, "apply changes (default: preview only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" || *ids == "" || flags.NArg() != 0 {
		return fmt.Errorf("--profile-file and --account-ids are required")
	}
	accountIDs, err := parseAccountIDs(*ids)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("cannot read profile file")
	}
	profile, err := claude.ParseCompatibilityProfile(string(raw))
	if err != nil {
		return err
	}
	opts, err := redis.ParseURL(os.Getenv(redisEnv))
	if err != nil {
		return fmt.Errorf("%s must contain a valid Redis URL", redisEnv)
	}
	opts.MaxRetries = -1
	rdb := redis.NewClient(opts)
	defer rdb.Close()
	failed := false
	for _, id := range accountIDs {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		fields, err := migrate(ctx, rdb, id, profile, *apply)
		cancel()
		if err != nil {
			fmt.Fprintf(out, "account=%d failed: %s\n", id, err)
			failed = true
			continue
		}
		mode := "preview"
		if *apply {
			mode = "applied"
		}
		fmt.Fprintf(out, "account=%d mode=%s changed_fields=%s\n", id, mode, strings.Join(fields, ","))
	}
	if failed {
		return fmt.Errorf("migration incomplete; review failed accounts before retrying")
	}
	return nil
}

func parseAccountIDs(raw string) ([]int64, error) {
	var result []int64
	seen := map[int64]bool{}
	for _, item := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(item), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("account IDs must be positive integers")
		}
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result, nil
}

func migrate(ctx context.Context, rdb *redis.Client, id int64, profile claude.CompatibilityProfile, apply bool) ([]string, error) {
	key := fmt.Sprintf("fingerprint:%d", id)
	raw, err := rdb.Get(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("cache unavailable or fingerprint missing")
	}
	ttl, err := rdb.PTTL(ctx, key).Result()
	if err != nil || ttl <= 0 {
		return nil, fmt.Errorf("fingerprint must have a positive remaining TTL")
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &data); err != nil || data == nil {
		return nil, fmt.Errorf("invalid cached fingerprint JSON")
	}
	for _, name := range []string{"ClientID", "UserAgent"} {
		var value string
		if json.Unmarshal(data[name], &value) != nil || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("cached fingerprint has invalid %s", name)
		}
	}
	values := map[string]string{
		"StainlessLang": profile.Lang, "StainlessPackageVersion": profile.PackageVersion,
		"StainlessOS": profile.OS, "StainlessArch": profile.Arch,
		"StainlessRuntime": profile.Runtime, "StainlessRuntimeVersion": profile.RuntimeVersion,
	}
	var changed []string
	for name, value := range values {
		var current string
		if json.Unmarshal(data[name], &current) == nil && current == value {
			continue
		}
		data[name], _ = json.Marshal(value)
		changed = append(changed, name)
	}
	sort.Strings(changed)
	if !apply || len(changed) == 0 {
		return changed, nil
	}
	replacement, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("cannot encode fingerprint")
	}
	if err := compareAndSet(ctx, rdb, key, raw, string(replacement)); err != nil {
		return nil, err
	}
	return changed, nil
}

func compareAndSet(ctx context.Context, rdb *redis.Client, key, original, replacement string) error {
	result, err := replaceProfile.Run(ctx, rdb, []string{key}, original, replacement).Int()
	if err != nil {
		return fmt.Errorf("Redis update failed; verify state before retrying")
	}
	if result != 1 {
		return fmt.Errorf("fingerprint changed, expired or lost its TTL; no update applied")
	}
	return nil
}
