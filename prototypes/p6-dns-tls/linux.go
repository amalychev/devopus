package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// linuxTests emulate a Linux host in a container: privileged ports for a non-root agent
// via setcap, and *.localhost through nss-myhostname (what systemd-resolved hosts get).
func linuxTests(p *pki) {
	section("linux", "Linux (эмуляция хоста в контейнере Debian 12)")

	probe := filepath.Join(p.Dir, "bindprobe")
	build := exec.Command("go", "build", "-o", probe, ".")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		record("linux", "сборка bindprobe", false, oneLine(string(out), err))
		return
	}

	// Docker sets ip_unprivileged_port_start=0 inside containers; restore the host default.
	base := []string{"run", "--rm", "--sysctl", "net.ipv4.ip_unprivileged_port_start=1024", "-v", p.Dir + ":/p6:ro", "debian:12-slim", "sh", "-c"}
	out, err := run("docker", append(base, `cp /p6/bindprobe /tmp/bp && chmod 755 /tmp/bp && su nobody -s /bin/sh -c "/tmp/bp 127.0.0.1:80"`)...)
	record("linux", "агент без root: bind 127.0.0.1:80", err != nil, "отказ: "+oneLine(out, err))
	out, err = run("docker", append(base, `apt-get -qq update >/dev/null && apt-get -qq install -y libcap2-bin >/dev/null 2>&1 &&
		cp /p6/bindprobe /tmp/bp && chmod 755 /tmp/bp && setcap cap_net_bind_service=+ep /tmp/bp &&
		su nobody -s /bin/sh -c "/tmp/bp 127.0.0.1:80 && /tmp/bp [::1]:80"`)...)
	record("linux", "агент без root + setcap cap_net_bind_service на бинарник: bind :80", err == nil, oneLine(out, err))

	out, err = run("docker", "run", "--rm", "debian:12-slim", "sh", "-c", "getent hosts probe.shop.localhost || echo 'не резолвится'")
	record("linux", "glibc без nss-myhostname: probe.shop.localhost", strings.Contains(out, "не резолвится"), oneLine(out, err))
	out, err = run("docker", "run", "--rm", "debian:12-slim", "sh", "-c", `apt-get -qq update >/dev/null && apt-get -qq install -y libnss-myhostname >/dev/null 2>&1 &&
		sed -i 's/^hosts:.*/hosts: files myhostname dns/' /etc/nsswitch.conf && getent ahosts probe.shop.localhost | awk '{print $1}' | sort -u | tr '\n' ' '`)
	record("linux", "glibc + nss-myhostname (как с systemd): probe.shop.localhost", strings.Contains(out, "127.0.0.1") || strings.Contains(out, "::1"), oneLine(out, err))
}
