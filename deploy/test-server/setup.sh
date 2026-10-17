#!/usr/bin/env bash
# One-time setup of the Devopus test server (docs/test-server.md): Auth and the
# workspace server on 127.0.0.1 behind the server's nginx, PostgreSQL in a container,
# and the autodeploy timer that builds every new commit of main in Docker (no Go on
# the server). Idempotent: running it again keeps the database and its password.
#
#   sudo SRC=/opt/devopus/src TLS=cert CERT=/etc/ssl/x.pem KEY=/etc/ssl/x.key bash deploy/test-server/setup.sh
#   sudo SRC=/opt/devopus/src ACME_EMAIL=you@example.com bash deploy/test-server/setup.sh   # certbot
set -euo pipefail

DOMAIN=${DOMAIN:-devopus.webapace.ink}
# One level under the zone (devopus-auth.webapace.ink): Cloudflare's free certificate
# covers *.webapace.ink, not *.devopus.webapace.ink.
AUTH_DOMAIN=${AUTH_DOMAIN:-${DOMAIN%%.*}-auth.${DOMAIN#*.}}
SRC=${SRC:-$(cd "$(dirname "$0")/../.." && pwd)}
BRANCH=${BRANCH:-main}
# Local ports: nginx proxies to them; nothing else listens on them.
AUTH_PORT=${AUTH_PORT:-7480}
SERVER_PORT=${SERVER_PORT:-7481}
PG_PORT=${PG_PORT:-5440}
# HTTPS:
#   cert    — nginx serves :443 with a certificate that covers both names (CERT, KEY),
#             e.g. a Cloudflare Origin Certificate for *.webapace.ink, as other sites do;
#   certbot — Let's Encrypt certificates through nginx (the names must not be proxied);
#   none    — only :80: Cloudflare's "Flexible" mode, or TLS set up by hand.
TLS=${TLS:-certbot}
CERT=${CERT:-}
KEY=${KEY:-}
ACME_EMAIL=${ACME_EMAIL:-}
# Login codes and invites: "log" (read them with journalctl) or smtp://user:pass@host:587.
MAIL=${MAIL:-log}
MAIL_FROM=${MAIL_FROM:-devopus@$DOMAIN}

CONF=/etc/devopus-test
STATE=/var/lib/devopus-test
LIB=/usr/local/lib/devopus-test
PG=devopus-test-postgres

say() { printf '\n== %s\n' "$*"; }
die() { echo "error: $*" >&2; exit 1; }
[ "$(id -u)" = 0 ] || die "run as root (sudo)"
[ -f "$SRC/go.mod" ] || die "SRC=$SRC is not a clone of devopus"
SRC=$(cd "$SRC" && pwd -P) # the timer runs from /: the path must be absolute
command -v docker >/dev/null || die "Docker is needed: the build runs in a container"
command -v nginx >/dev/null || die "nginx is needed: it serves 80/443 and proxies to the services"
case "$TLS" in
  certbot) [ -n "$ACME_EMAIL" ] || die "ACME_EMAIL is needed for Let's Encrypt" ;;
  cert) [ -r "$CERT" ] && [ -r "$KEY" ] || die "TLS=cert needs CERT and KEY: readable files of the certificate and its key" ;;
  none) ;;
  *) die "TLS is cert, certbot or none" ;;
esac

say "ports 127.0.0.1:$AUTH_PORT (Auth), :$SERVER_PORT (workspace server), :$PG_PORT (PostgreSQL)"
busy() { # port, the process that may hold it already
  local who
  who=$(ss -ltnpH "( sport = :$1 )" 2>/dev/null | head -1)
  [ -n "$who" ] && ! grep -q "$2" <<<"$who"
}
busy "$AUTH_PORT" devopus-auth && die "port $AUTH_PORT is taken: set AUTH_PORT"
busy "$SERVER_PORT" devopus && die "port $SERVER_PORT is taken: set SERVER_PORT"
busy "$PG_PORT" docker-proxy && die "port $PG_PORT is taken: set PG_PORT"

say "packages: git, curl"
export DEBIAN_FRONTEND=noninteractive
command -v git >/dev/null && command -v curl >/dev/null || { apt-get update -qq && apt-get install -y -qq git curl >/dev/null; }
if [ "$TLS" = certbot ] && ! command -v certbot >/dev/null; then
  apt-get update -qq && apt-get install -y -qq certbot python3-certbot-nginx >/dev/null
fi

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
DEVOPUS_AUTH_LISTEN=127.0.0.1:$AUTH_PORT
DEVOPUS_AUTH_ISSUER=https://$AUTH_DOMAIN
DEVOPUS_AUTH_MAIL=$MAIL
DEVOPUS_AUTH_MAIL_FROM=$MAIL_FROM
DEVOPUS_AUTH_TRUST_PROXY=1
EOF
cat > "$CONF/server.env" <<EOF
DEVOPUS_SERVER_DATABASE_URL=$(url devopus_server)
DEVOPUS_SERVER_LISTEN=127.0.0.1:$SERVER_PORT
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
AUTH_PORT=$AUTH_PORT
SERVER_PORT=$SERVER_PORT
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
After=network-online.target docker.service

[Service]
Type=oneshot
EnvironmentFile=$CONF/deploy.env
ExecStart=/bin/bash $LIB/deploy.sh
TimeoutStartSec=30min
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
systemctl daemon-reload
systemctl enable devopus-auth devopus-server >/dev/null

say "the first deploy (the first build in Docker takes a few minutes)"
FORCE=1 SRC=$SRC BRANCH=$BRANCH DOMAIN=$DOMAIN AUTH_PORT=$AUTH_PORT SERVER_PORT=$SERVER_PORT bash "$LIB/deploy.sh"
systemctl enable --now devopus-deploy.timer >/dev/null

say "nginx: $DOMAIN and $AUTH_DOMAIN"
if [ -d /etc/nginx/sites-enabled ]; then
  site=/etc/nginx/sites-available/devopus-test
  ln -sf "$site" /etc/nginx/sites-enabled/devopus-test
else
  site=/etc/nginx/conf.d/devopus-test.conf
fi
proxy() { # port
  cat <<EOF
        proxy_pass http://127.0.0.1:$1;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        # Clients always come over https: through Cloudflare or nginx's own TLS.
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header Connection "";
        # Agents keep a stream of settings open (Watch): no buffering, long reads.
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
        client_max_body_size 10m;
EOF
}
listen() {
  echo "    listen 80;"
  echo "    listen [::]:80;"
  if [ "$TLS" = cert ]; then
    echo "    listen 443 ssl;"
    echo "    listen [::]:443 ssl;"
    echo "    ssl_certificate $CERT;"
    echo "    ssl_certificate_key $KEY;"
  fi
}
# certbot adds the 443 parts and the redirect to these blocks; rewriting the file
# drops them, so with certbot an existing file is kept unless REWRITE_NGINX=1.
if [ "$TLS" != certbot ] || [ ! -f "$site" ] || [ "${REWRITE_NGINX:-}" = 1 ]; then
  cat > "$site" <<EOF
# Devopus test server (deploy/test-server/setup.sh, docs/test-server.md)
server {
$(listen)
    server_name $DOMAIN;
    # Builds of the CLI of the deployed commit.
    location /dl/ {
        alias $STATE/dl/;
        autoindex on;
    }
    location / {
$(proxy "$SERVER_PORT")
    }
}

server {
$(listen)
    server_name $AUTH_DOMAIN;
    location / {
$(proxy "$AUTH_PORT")
    }
}
EOF
fi
nginx -t
systemctl reload nginx
if [ "$TLS" = certbot ]; then
  certbot --nginx --non-interactive --agree-tos -m "$ACME_EMAIL" --redirect -d "$DOMAIN" -d "$AUTH_DOMAIN"
fi

say "done"
echo "workspace server: https://$DOMAIN/healthz -> $(curl -fsS "https://$DOMAIN/healthz" || echo FAIL)"
echo "Auth:             https://$AUTH_DOMAIN/healthz -> $(curl -fsS "https://$AUTH_DOMAIN/healthz" || echo FAIL)"
echo "deployed:         $(cat $STATE/deployed)"
