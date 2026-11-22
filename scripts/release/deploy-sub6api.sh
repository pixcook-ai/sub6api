#!/usr/bin/env bash
# Deploys only the isolated Sub6API app; database/cache and other sites stay running.
set -euo pipefail
release=${1:?release directory required}
root=/opt/sub6api
exec 9>"$root/deploy.lock"
flock -n 9 || { echo 'Another deployment is running' >&2; exit 1; }
cd "$release"
sha256sum -c SHA256SUMS
commit=$(cat COMMIT)
[[ "$commit" =~ ^[a-f0-9]{40}$ ]] || exit 1
[[ "$release" == "$root/releases/$commit" ]] || exit 1
test -f "$root/MIGRATIONS_SHA256" || { echo 'Record the verified deployed migration baseline first' >&2; exit 1; }
cmp MIGRATIONS_SHA256 "$root/MIGRATIONS_SHA256" || { echo 'Schema changed: dedicated migration/restore plan required' >&2; exit 1; }
test -f "$root/compose.json"
mkdir -p "$root/logs"
previous=$(docker inspect sub6api-app-1 --format '{{.Image}}')
docker image inspect "$previous" >/dev/null
cp "$root/compose.json" "$release/compose.before.json"
chmod 600 "$release/compose.before.json"
# Backup must complete before changing the app image.
systemctl start sub6api-backup.service
test "$(systemctl show sub6api-backup.service -p Result --value)" = success
mkdir -p runtime
cp sub2api runtime/sub2api
chmod 755 runtime/sub2api
tar -xzf resources.tar.gz -C runtime
printf 'FROM %s\nCOPY --chown=1000:1000 sub2api /app/sub2api\nCOPY --chown=1000:1000 resources /app/resources\n' "$previous" > runtime/Dockerfile
image="sub6api:sha-$commit"
docker build --network=none --label "org.opencontainers.image.revision=$commit" -t "$image" runtime
rollback() {
  cp "$release/compose.before.json" "$root/compose.json"
  docker compose -p sub6api -f "$root/compose.json" up -d --no-deps --pull never app
}
switched=false
finish() {
  code=$?
  if [[ "$code" != 0 && "$switched" == true ]]; then
    echo 'Deployment failed; restoring previous app image' >&2
    rollback || echo 'Rollback failed: operator action required' >&2
  fi
  exit "$code"
}
trap finish EXIT
python3 - "$root/compose.json" "$image" <<'PY'
import json,os,sys
p=sys.argv[1]
with open(p) as f: c=json.load(f)
c['services']['app']['image']=sys.argv[2]
tmp=p+'.next'
with open(tmp,'w') as f: json.dump(c,f,indent=2)
os.chmod(tmp,0o600)
os.replace(tmp,p)
PY
switched=true
docker compose -p sub6api -f "$root/compose.json" up -d --no-deps --pull never app
healthy=false
for _ in $(seq 1 60); do
  if [[ "$(docker inspect sub6api-app-1 --format '{{.State.Health.Status}}')" == healthy ]]; then healthy=true; break; fi
  sleep 2
done
[[ "$healthy" == true ]]
expected=$(sha256sum sub2api | cut -d' ' -f1)
actual=$(docker exec --user 1000:1000 sub6api-app-1 sha256sum /proc/1/exe | cut -d' ' -f1)
[[ "$expected" == "$actual" ]]
curl --fail --silent --show-error --retry 3 https://sub6api.pixcook.ai/health | python3 -c 'import json,sys; assert json.load(sys.stdin)["status"] == "ok"'
python3 - "$root/logs/deployment-$commit.json" "$commit" "$image" "$expected" "$previous" <<'PY'
import json,sys,datetime
with open(sys.argv[1],'w') as f: json.dump(dict(commit=sys.argv[2],image=sys.argv[3],binary_sha256=sys.argv[4],previous_image=sys.argv[5],verified_at=datetime.datetime.now(datetime.timezone.utc).isoformat()),f,indent=2)
PY
printf '%s\n' "$commit" > "$root/SOURCE_COMMIT"
switched=false
echo "Deployed verified commit $commit"
