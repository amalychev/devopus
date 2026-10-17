#!/usr/bin/env bash
# Autodeploy of the Devopus test server (docs/test-server.md). The timer runs it every
# minute: when the branch has a new commit, it builds Auth and the workspace server,
# installs them, restarts them and checks /healthz; if they do not come up, the
# previous binaries come back and the commit is not tried again. It also publishes
# the CLI of that commit at https://<domain>/dl/.
#
#   sudo systemctl start devopus-deploy        # now, instead of within a minute
#   sudo FORCE=1 bash /usr/local/lib/devopus-test/deploy.sh   # the same commit again
#   journalctl -u devopus-deploy -n 100        # what it did
set -euo pipefail

SRC=${SRC:?the clone of devopus}
BRANCH=${BRANCH:-main}
DOMAIN=${DOMAIN:-devopus.webapace.ink}
STATE=/var/lib/devopus-test
LIB=/usr/local/lib/devopus-test
export PATH=/usr/local/go/bin:$PATH HOME=${HOME:-/root}
export GOPATH=$STATE/go GOCACHE=$STATE/go-cache GOFLAGS=-buildvcs=false

exec 9>"$STATE/deploy.lock"
flock -n 9 || { echo "a deploy is running"; exit 0; }

# The clone belongs to whoever cloned it, with their access to GitHub.
owner=$(stat -c %U "$SRC")
as_owner() { if [ "$owner" = root ]; then "$@"; else sudo -u "$owner" -H "$@"; fi; }
git_() { as_owner git -C "$SRC" "$@"; }

git_ fetch -q origin "$BRANCH"
next=$(git_ rev-parse "origin/$BRANCH")
deployed=$(cat "$STATE/deployed" 2>/dev/null || true)
failed=$(cat "$STATE/failed" 2>/dev/null || true)
if [ -z "${FORCE:-}" ] && { [ "$next" = "$deployed" ] || [ "$next" = "$failed" ]; }; then
  exit 0
fi

echo "deploying $next (was ${deployed:-nothing})"
git_ checkout -q -f --detach "$next"
cd "$SRC"
version="test-$(git_ describe --tags --always)"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
build() { # output, package, version variable
  CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X $3=$version" -o "$1" "$2"
}
build "$out/devopus" ./cmd/devopus github.com/amalychev/devopus/internal/cli.Version
build "$out/devopus-auth" ./cmd/devopus-auth main.Version

healthy() {
  for _ in $(seq 60); do
    if curl -fsS -m 2 http://127.0.0.1:7480/healthz >/dev/null 2>&1 && curl -fsS -m 2 http://127.0.0.1:7481/healthz >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

for b in devopus devopus-auth; do
  [ -f "/usr/local/bin/$b" ] && cp -f "/usr/local/bin/$b" "$LIB/$b.prev"
  install -m 0755 "$out/$b" "/usr/local/bin/$b"
done
systemctl restart devopus-auth devopus-server
if ! healthy; then
  echo "$next does not come up: rolling back"
  journalctl -u devopus-auth -u devopus-server -n 40 --no-pager || true
  for b in devopus devopus-auth; do
    [ -f "$LIB/$b.prev" ] && install -m 0755 "$LIB/$b.prev" "/usr/local/bin/$b"
  done
  systemctl restart devopus-auth devopus-server
  healthy || echo "the previous version does not come up either"
  echo "$next" > "$STATE/failed"
  exit 1
fi
echo "$next" > "$STATE/deployed"
rm -f "$STATE/failed"

# The CLI of this commit for testers' machines.
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  GOOS=${target%/*} GOARCH=${target#*/} build "$out/devopus_${target%/*}_${target#*/}" ./cmd/devopus github.com/amalychev/devopus/internal/cli.Version
  install -m 0644 "$out/devopus_${target%/*}_${target#*/}" "$STATE/dl/"
done
printf '%s\n%s\n' "$version" "$next" > "$STATE/dl/version.txt"

# The next run uses this commit's deploy script; a new file, so this run is unaffected.
install -m 0755 "$SRC/deploy/test-server/deploy.sh" "$LIB/deploy.sh"
echo "deployed $version: https://$DOMAIN"
