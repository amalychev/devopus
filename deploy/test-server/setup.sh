#!/usr/bin/env bash
# One-time setup of the Devopus test server (docs/test-server.md): Auth and the
# workspace server behind Caddy with HTTPS, PostgreSQL in a container, and the
# autodeploy timer that builds and rolls out every new commit of main.
# Idempotent: running it again keeps the database and its password.
#
#   sudo DOMAIN=devopus.webapace.ink SRC=/opt/devopus/src bash deploy/test-server/setup.sh
set -euo pipefail

DOMAIN=${DOMAIN:-devopus.webapace.ink}
AUTH_DOMAIN=${AUTH_DOMAIN:-auth.$DOMAIN}
SRC=${SRC:-$(cd "$(dirname "$0")/../.." && pwd)}
BRANCH=${BRANCH:-main}
ACME_EMAIL=${ACME_EMAIL:-}
# Login codes and invites: "log" (read them with journalctl) or smtp://user:pass@host:587.
MAIL=${MAIL:-log}
MAIL_FROM=${MAIL_FROM:-devopus@$DOMAIN}

CONF=/etc/devopus-test
STATE=/var/lib/devopus-test
LIB=/usr/local/lib/devopus-test
PG=devopus-test-postgres
PG_PORT=5440

say() { printf '\n== %s\n' "$*"; }
[ "$(id -u)" = 0 ] || { echo "run as root (sudo)"; exit 1; }
[ -f "$SRC/go.mod" ] || { echo "SRC=$SRC is not a clone of devopus"; exit 1; }

say "packages: git, curl, Docker, Caddy"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq git curl ca-certificates gnupg debian-keyring debian-archive-keyring apt-transport-https >/dev/null
if ! command -v docker >/dev/null; then
  curl -fsSL https://get.docker.com | sh
fi
if ! command -v caddy >/dev/null; then
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq && apt-get install -y -qq caddy >/dev/null
fi

say "Go (the version of go.mod)"
want=$(awk '/^go /{print $2}' "$SRC/go.mod")
if [ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null)" != "go$want" ]; then
  arch=$(dpkg --print-architecture)
  curl -fsSL "https://go.dev/dl/go$want.linux-$arch.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
fi
/usr/local/go/bin/go version

say "user and folders"
id devopus >/dev/null 2>&1 || useradd --system --home-dir "$STATE" --shell /usr/sbin/nologin devopus
install -d -m 0755 "$STATE" "$LIB" "$STATE/dl"
install -d -m 0750 -o devopus -g devopus "$STATE/server"
install -d -m 0750 "$CONF"

say "PostgreSQL in $PG on 127.0.0.1:$PG_PORT"
if [ ! -f "$CONF/postgres.env" ]; then
  umask 077
  echo "POSTGRES_PASSWORD=$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')" > "$CONF/postgres.env"
  umask 022
fi
. "$CONF/postgres.env"
if ! docker inspect "$PG" >/dev/null 2>&1; then
  docker run -d --name "$PG" --restart unless-stopped --env-file "$CONF/postgres.env" \
    -v devopus-test-pgdata:/var/lib/postgresql/data -p "127.0.0.1:$PG_PORT:5432" postgres:17 >/dev/null
fi
docker start "$PG" >/dev/null
for _ in $(seq 60); do docker exec "$PG" pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
for db in devopus_auth devopus_server; do
  docker exec "$PG" psql -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='$db'" | grep -q 1 ||
    docker exec "$PG" psql -U postgres -qc "CREATE DATABASE $db"
done
url() { echo "postgres://postgres:$POSTGRES_PASSWORD@127.0.0.1:$PG_PORT/$1?sslmode=disable"; }

say "settings in $CONF"
umask 077
cat > "$CONF/auth.env" <<EOF
DEVOPUS_AUTH_DATABASE_URL=$(url devopus_auth)
DEVOPUS_AUTH_LISTEN=127.0.0.1:7480
DEVOPUS_AUTH_ISSUER=https://$AUTH_DOMAIN
DEVOPUS_AUTH_MAIL=$MAIL
DEVOPUS_AUTH_MAIL_FROM=$MAIL_FROM
DEVOPUS_AUTH_TRUST_PROXY=1
EOF
cat > "$CONF/server.env" <<EOF
DEVOPUS_SERVER_DATABASE_URL=$(url devopus_server)
DEVOPUS_SERVER_LISTEN=127.0.0.1:7481
DEVOPUS_SERVER_PUBLIC_URL=https://$DOMAIN
DEVOPUS_AUTH_URL=https://$AUTH_DOMAIN
DEVOPUS_SERVER_MAIL=$MAIL
DEVOPUS_SERVER_MAIL_FROM=$MAIL_FROM
DEVOPUS_SERVER_DATA=$STATE/server
EOF
cat > "$CONF/deploy.env" <<EOF
SRC=$SRC
BRANCH=$BRANCH
DOMAIN=$DOMAIN
EOF
umask 022

say "services"
unit() { # name, description, binary and arguments, env file
  cat > "/etc/systemd/system/$1.service" <<EOF
[Unit]
Description=$2
After=network-online.target docker.service
Wants=network-online.target

[Service]
User=devopus
EnvironmentFile=$4
ExecStart=$3
Restart=always
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$STATE/server
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF
}
unit devopus-auth "Devopus Auth (test server)" "/usr/local/bin/devopus-auth run" "$CONF/auth.env"
unit devopus-server "Devopus workspace server (test server)" "/usr/local/bin/devopus server run" "$CONF/server.env"

cat > /etc/systemd/system/devopus-deploy.service <<EOF
[Unit]
Description=Build and roll out the newest commit of $BRANCH (Devopus test server)
After=network-online.target

[Service]
Type=oneshot
EnvironmentFile=$CONF/deploy.env
ExecStart=/bin/bash $LIB/deploy.sh
TimeoutStartSec=20min
EOF
cat > /etc/systemd/system/devopus-deploy.timer <<EOF
[Unit]
Description=Check $BRANCH for new commits every minute

[Timer]
OnCalendar=*:*:00
AccuracySec=5s

[Install]
WantedBy=timers.target
EOF
install -m 0755 "$SRC/deploy/test-server/deploy.sh" "$LIB/deploy.sh"

say "Caddy: https://$DOMAIN and https://$AUTH_DOMAIN"
global=""
[ -n "$ACME_EMAIL" ] && global="{
	email $ACME_EMAIL
}
"
cat > /etc/caddy/Caddyfile <<EOF
${global}$DOMAIN {
	encode zstd gzip
	# Builds of the CLI of the deployed commit.
	handle_path /dl/* {
		root * $STATE/dl
		file_server browse
	}
	handle {
		reverse_proxy 127.0.0.1:7481 {
			flush_interval -1
		}
	}
}

$AUTH_DOMAIN {
	encode zstd gzip
	reverse_proxy 127.0.0.1:7480
}
EOF
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null
systemctl daemon-reload
systemctl enable --now caddy >/dev/null
systemctl reload caddy

say "the first deploy"
systemctl enable devopus-auth devopus-server >/dev/null
FORCE=1 SRC=$SRC BRANCH=$BRANCH DOMAIN=$DOMAIN bash "$LIB/deploy.sh"
systemctl enable --now devopus-deploy.timer >/dev/null

say "done"
echo "workspace server: https://$DOMAIN/healthz -> $(curl -fsS https://$DOMAIN/healthz || echo FAIL)"
echo "Auth:             https://$AUTH_DOMAIN/healthz -> $(curl -fsS https://$AUTH_DOMAIN/healthz || echo FAIL)"
echo "deployed:         $(cat $STATE/deployed)"
