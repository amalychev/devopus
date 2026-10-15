package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// hostTests runs on the developer machine without root and without changing any system
// setting: resolution of *.localhost, privileged ports, IPv6, and TLS trust per client.
func hostTests(p *pki) {
	section("host", "Хост (macOS), без root и без системных изменений")

	// 1. Who resolves *.localhost through the system resolver, with no DNS configuration?
	name := "probe.shop.localhost"
	stopProbe := serveText("127.0.0.1:18499", "probe")
	defer stopProbe()
	javaR := filepath.Join(p.Dir, "R.java")
	_ = os.WriteFile(javaR, []byte(`class R { public static void main(String[] a) throws Exception {
  for (var x : java.net.InetAddress.getAllByName(a[0])) System.out.print(x.getHostAddress() + " "); } }`), 0o644)
	clients := []struct{ who, cmd string }{
		{"curl", `curl -s -o /dev/null -w '%{remote_ip}' http://` + name + `:18499/`},
		{"Python socket.getaddrinfo", `python3 -c "import socket;print(sorted({a[4][0] for a in socket.getaddrinfo('` + name + `',80)}))"`},
		{"Node dns.lookup", `node -e "require('dns').lookup('` + name + `',{all:true},(e,a)=>console.log(e?e.code:a.map(x=>x.address).join(',')))"`},
		{"PHP gethostbynamel", `php -r 'echo implode(",", gethostbynamel("` + name + `") ?: ["не резолвится"]);'`},
		{"Java InetAddress", "java " + javaR + " " + name},
	}
	for _, c := range clients {
		out, err := shell(c.cmd)
		out = strings.TrimSpace(out)
		ok := err == nil && (strings.Contains(out, "127.0.0.1") || strings.Contains(out, "::1"))
		record("host", "резолвинг "+name+": "+c.who, ok, oneLine(out, err))
	}
	if addrs, err := net.LookupHost(name); err == nil {
		record("host", "резолвинг "+name+": Go net.LookupHost", true, strings.Join(addrs, ", "))
	} else {
		record("host", "резолвинг "+name+": Go net.LookupHost", false, err.Error())
	}

	// 2. Privileged ports as a normal user.
	for _, addr := range []string{"127.0.0.1:81", "0.0.0.0:81", "[::1]:81", "[::]:81"} {
		l, err := net.Listen("tcp", addr)
		if err == nil {
			l.Close()
			record("host", "bind "+addr+" без root", true, "получилось")
		} else {
			var errno syscall.Errno
			msg := err.Error()
			if errors.As(err, &errno) {
				msg = errno.Error()
			}
			record("host", "bind "+addr+" без root", false, "отказ: "+msg)
		}
	}

	// 3. Docker Desktop publishes privileged ports on loopback through its own helper.
	for _, pub := range []string{"127.0.0.1:81:80", "[::1]:81:80"} {
		_, _ = run("docker", "rm", "-f", "p6-pub")
		_, err := run("docker", "run", "-d", "--name", "p6-pub", "-p", pub, "python:3.12-slim", "python", "-m", "http.server", "80")
		hostAddr := strings.TrimSuffix(pub, ":80")
		got := "не опубликовалось"
		ok := false
		if err == nil {
			for i := 0; i < 30 && !ok; i++ {
				time.Sleep(300 * time.Millisecond)
				resp, e := (&http.Client{Timeout: time.Second}).Get("http://" + hostAddr + "/")
				if e == nil {
					resp.Body.Close()
					ok = true
					got = fmt.Sprintf("отвечает %d", resp.StatusCode)
				}
			}
		} else {
			got = oneLine("", err)
		}
		record("host", "Docker Desktop: -p "+pub, ok, got)
		_, _ = run("docker", "rm", "-f", "p6-pub")
	}

	// 4. IPv6 shadowing: Devcorb listens on 127.0.0.1 only, another program on ::1.
	const port = 18500
	stopA := serveText(fmt.Sprintf("127.0.0.1:%d", port), "devcorb")
	stopB := serveText(fmt.Sprintf("[::1]:%d", port), "чужой сервер на ::1")
	for _, c := range []struct{ who, cmd string }{
		{"curl", fmt.Sprintf("curl -s http://app.shop.localhost:%d/", port)},
		{"Python urllib", fmt.Sprintf(`python3 -c "import urllib.request;print(urllib.request.urlopen('http://app.shop.localhost:%d/').read().decode())"`, port)},
		{"Node fetch", fmt.Sprintf(`node -e "fetch('http://app.shop.localhost:%d/').then(r=>r.text()).then(console.log)"`, port)},
	} {
		out, err := shell(c.cmd)
		out = strings.TrimSpace(out)
		record("host", "IPv6: Devcorb на 127.0.0.1, чужой на ::1 → "+c.who, out == "devcorb", "ответил: "+oneLine(out, err))
	}
	stopA()
	stopB()

	// 5. TLS: our CA is trusted per client, and Name Constraints stop it from vouching
	// for any non-localhost name. Variant 2 adds a constrained intermediate.
	tlsChecks(p, "один корневой CA", p.CAPath, &p.Good, &p.Evil, 18501)
	tlsChecks(p, "корень + промежуточный CA", p.Root2Path, &p.Good2, &p.Evil2, 18502)
}

func tlsChecks(p *pki, variant, caPath string, good, evil *tls.Certificate, tlsPort int) {
	stop := serveTLSCerts(fmt.Sprintf("127.0.0.1:%d", tlsPort), good, evil)
	defer stop()
	hosts := filepath.Join(p.Dir, "hosts")
	_ = os.WriteFile(hosts, []byte("127.0.0.1 app.shop.localhost evil.example.com\n"), 0o644)
	store := filepath.Join(p.Dir, fmt.Sprintf("truststore-%d.p12", tlsPort))
	_ = os.Remove(store)
	_, keytoolErr := run("keytool", "-importcert", "-noprompt", "-alias", "devcorb", "-file", caPath,
		"-keystore", store, "-storetype", "PKCS12", "-storepass", "changeit")
	javaSrc := filepath.Join(p.Dir, "Get.java")
	_ = os.WriteFile(javaSrc, []byte(`class Get { public static void main(String[] a) throws Exception {
  var c = java.net.http.HttpClient.newHttpClient();
  var r = c.send(java.net.http.HttpRequest.newBuilder(java.net.URI.create(a[0])).build(), java.net.http.HttpResponse.BodyHandlers.ofString());
  System.out.print(r.body()); } }`), 0o644)

	type client struct {
		who string
		cmd func(name string) string
	}
	tlsClients := []client{
		{"curl (SecureTransport) --cacert", func(n string) string {
			return fmt.Sprintf("curl -sS --cacert %s --resolve %s:%d:127.0.0.1 https://%s:%d/", caPath, n, tlsPort, n, tlsPort)
		}},
		{"Python ssl, cafile", func(n string) string {
			return fmt.Sprintf(`python3 -c "import ssl,socket;c=ssl.create_default_context(cafile='%s');s=c.wrap_socket(socket.create_connection(('127.0.0.1',%d)),server_hostname='%s');print('ok')"`, caPath, tlsPort, n)
		}},
		{"Node, NODE_EXTRA_CA_CERTS", func(n string) string {
			return fmt.Sprintf(`NODE_EXTRA_CA_CERTS=%s node -e "const s=require('tls').connect({host:'127.0.0.1',port:%d,servername:'%s'},()=>{console.log('ok');s.end()});s.on('error',e=>{console.log(e.code);process.exit(1)})"`, caPath, tlsPort, n)
		}},
		{"PHP openssl, cafile", func(n string) string {
			return fmt.Sprintf(`php -r '$c=stream_context_create(["ssl"=>["cafile"=>"%s","peer_name"=>"%s","verify_peer"=>true]]);$s=@stream_socket_client("ssl://127.0.0.1:%d",$e,$m,3,STREAM_CLIENT_CONNECT,$c);if(!$s){echo error_get_last()["message"]??"fail";exit(1);}echo "ok";'`, caPath, n, tlsPort)
		}},
		{"Java, truststore PKCS12", func(n string) string {
			return fmt.Sprintf("java -Djdk.net.hosts.file=%s -Djavax.net.ssl.trustStore=%s -Djavax.net.ssl.trustStorePassword=changeit %s https://%s:%d/", hosts, store, javaSrc, n, tlsPort)
		}},
	}
	for _, c := range tlsClients {
		if strings.HasPrefix(c.who, "Java") && keytoolErr != nil {
			record("host", variant+": TLS "+c.who, false, "keytool: "+keytoolErr.Error())
			continue
		}
		out, err := shell(c.cmd("app.shop.localhost"))
		record("host", variant+", app.shop.localhost: "+c.who, err == nil && strings.Contains(out, "ok"), oneLine(out, err))
		out, err = shell(c.cmd("evil.example.com"))
		rejected := err != nil || !strings.Contains(out, "ok")
		detail := "отклонён: " + oneLine(out, err)
		if !rejected {
			detail = "ПРИНЯТ — Name Constraints не проверены"
		}
		record("host", variant+", evil.example.com: "+c.who, rejected, detail)
	}
	pool := x509.NewCertPool()
	pem, _ := os.ReadFile(caPath)
	pool.AppendCertsFromPEM(pem)
	for _, n := range []string{"app.shop.localhost", "evil.example.com"} {
		conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tlsPort), &tls.Config{RootCAs: pool, ServerName: n})
		if conn != nil {
			conn.Close()
		}
		if n == "app.shop.localhost" {
			record("host", variant+", app.shop.localhost: Go crypto/tls", err == nil, oneLine("ok", err))
		} else {
			record("host", variant+", evil.example.com: Go crypto/tls", err != nil, "отклонён: "+oneLine("", err))
		}
	}
}

func serveText(addr, text string) func() {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return func() {}
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, text) })}
	go srv.Serve(l)
	return func() { srv.Close() }
}

func serveTLSCerts(addr string, good, evil *tls.Certificate) func() {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return func() {}
	}
	cfg := &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if h.ServerName == "evil.example.com" {
			return evil, nil
		}
		return good, nil
	}}
	srv := &http.Server{
		Handler:  http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }),
		ErrorLog: log.New(io.Discard, "", 0), // rejected handshakes are the point of the test
	}
	go srv.Serve(tls.NewListener(l, cfg))
	return func() { srv.Close() }
}

func shell(cmd string) (string, error) {
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	return string(out), err
}

func oneLine(out string, err error) string {
	s := strings.TrimSpace(out)
	if s == "" && err != nil {
		s = err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 110 {
		s = s[:110] + "…"
	}
	return s
}
