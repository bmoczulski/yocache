---
title: Behind a reverse proxy (HTTPS)
description: Serve YoCache over HTTPS, or run several instances under one hostname, behind nginx, with the settings that sstate uploads and hash equivalence over WebSocket need.
---

YoCache itself speaks plain HTTP. To serve it over HTTPS, or to run several
instances under one hostname (`https://cache.example.com/ci/`,
`https://cache.example.com/dev/`), put a reverse proxy in front of it.
Everything a build does goes through the one HTTP port, 6768: downloads,
sstate, uploads, the dashboard, and hash equivalence over WebSocket. So one
proxy rule per instance covers it all.

## nginx example

Two instances under prefixes on one HTTPS hostname, nothing served at the
root. The site itself, as `conf.d/cache.example.com.conf`:

```nginx
map $http_upgrade $connection_upgrade { default upgrade; '' ''; }

server {
    listen 443 ssl;
    server_name cache.example.com;
    ssl_certificate     /etc/ssl/cache.example.com.pem;
    ssl_certificate_key /etc/ssl/cache.example.com.key;

    client_max_body_size 0;      # sstate archives can be hundreds of MB
    proxy_read_timeout   1h;     # hash-equivalence WebSockets idle between tasks

    location /ci/  { include snippets/proxy-params.conf; proxy_pass http://yocache-ci:6768/; }
    location /dev/ { include snippets/proxy-params.conf; proxy_pass http://yocache-dev:6768/; }
    location /     { return 404; }
}
```

and the proxy settings every instance's `location` includes, as
`snippets/proxy-params.conf`:

```nginx
proxy_http_version 1.1;                           # needed for WebSocket
proxy_set_header Upgrade    $http_upgrade;        # pass the WebSocket upgrade
proxy_set_header Connection $connection_upgrade;
proxy_set_header Host $host;
proxy_request_buffering off;                      # stream uploads through
```

For a single instance on its own hostname, use `location / { … }` with the
same settings.

### Why each setting matters

- **`client_max_body_size 0`**: nginx's default limit is 1 MB, and it rejects
  anything larger with `413` before YoCache sees it. Without this line, every
  sstate archive over 1 MB fails to upload.
- **`proxy_http_version 1.1` and the `Upgrade`/`Connection` headers**: hash
  equivalence runs over a WebSocket at `/hashequiv`, which starts as an HTTP
  upgrade request. nginx doesn't forward the upgrade by default. YoCache then
  answers `426 Upgrade Required` and the build runs without hash
  equivalence.
- **`proxy_read_timeout`**: nginx closes a WebSocket that has been idle for
  60 seconds by default. bitbake reconnects, so builds keep working, but they
  log `Error talking to server: Connection closed` each time.
- **`proxy_request_buffering off`**: streams uploads to YoCache instead of
  writing each archive to the proxy's disk first.
- **Trailing slashes** on both `location /ci/` and `proxy_pass …:6768/`: this
  is what strips the prefix, so YoCache receives `/sstate/…`, not
  `/ci/sstate/…`. nginx also redirects a bare `/ci` to `/ci/` on its own.
  The [dashboard](../server-configuration/#web-dashboard) works under a
  prefix as is.

## Client configuration

Point `YOCACHE_URL` at the proxy, prefix included:

```
YOCACHE_URL = "https://cache.example.com/ci"
```

Mirrors, uploads, the end-of-build summary and hash equivalence all follow
it. The layer derives `BB_HASHSERVE = "wss://cache.example.com/ci/hashequiv"`
itself.

On releases before Scarthgap, bitbake's hash-equivalence client can't speak
WebSocket, and the layer falls back to YoCache's raw-TCP listener on port
6767. That isn't HTTP, so it can't go through the proxy's `location` rules,
but nginx's `stream` module can relay it as plain TCP:

```nginx
stream {
    server {
        listen 6767;
        proxy_pass yocache-ci:6767;
        proxy_timeout 1h;
    }
}
```

The `stream` block goes at the top level of `nginx.conf`, next to `http`.
On these releases the layer defaults `BB_HASHSERVE` to `YOCACHE_URL`'s
hostname with port 6767, so a relay on that port needs no client setting;
on any other port, set `BB_HASHSERVE = "cache.example.com:<port>"`. The
relay has three limits: bitbake's raw-TCP client has no TLS, so the
connection stays unencrypted; TCP has no paths to route by, so each instance
needs its own port; and YoCache sees the relay's address, not the
builder's.

## Private or self-signed certificates

If the proxy's certificate isn't from a public CA, the build host has to
trust your CA. **Add the CA to the build host's system trust store**
(`update-ca-certificates` on Debian and Ubuntu), or to the image if you build
in a container such as kas-container. Every tool a build uses reads the
system store, so nothing else is needed.

If you can't change the trust store, you need both of these, because the two
halves of a build look for certificates in different places:

- **bitbake's Python code** (mirror checks, uploads, hash equivalence) reads
  `SSL_CERT_FILE`. bitbake removes most environment variables at startup,
  so let this one through:
  ```sh
  export SSL_CERT_FILE=/path/to/ca.pem
  export BB_ENV_PASSTHROUGH_ADDITIONS="$BB_ENV_PASSTHROUGH_ADDITIONS SSL_CERT_FILE"
  ```
- **`wget`**, which bitbake runs to download mirror objects, ignores
  `SSL_CERT_FILE` when built with GnuTLS, as it is on Debian and Ubuntu.
  Pass the CA explicitly:
  ```
  FETCHCMD_wget = "/usr/bin/env wget --tries=2 --timeout=100 --ca-certificate=/path/to/ca.pem"
  ```
  Without this line the mirror check still finds the objects, but every
  download fails with `The certificate of '…' is not trusted` and bitbake
  rebuilds them instead.

If you make your own CA, give its certificate the `basicConstraints = CA:TRUE`
and `keyUsage = keyCertSign` extensions. Python 3.13 and later reject a CA
certificate without them.

## Known limitations

- **Raw-TCP hash equivalence doesn't go through the HTTPS proxy.** Releases
  before Scarthgap can't use the WebSocket endpoint and need YoCache's raw-TCP
  listener instead. An nginx `stream` relay can carry it (see [Client
  configuration](#client-configuration)), but only unencrypted, on its own
  port per instance, and with the relay's address in YoCache's logs.
- **Duplicate uploads still send their data.** The uploader asks with
  `Expect: 100-continue`, so that YoCache can refuse an object it already has
  before the body is sent. nginx answers `100 Continue` itself, so behind it
  the whole archive is sent before YoCache gets to refuse it. Uploads stay
  correct; they just cost more bandwidth when several builders upload the
  same objects.
- **Logs show the proxy's address.** YoCache doesn't read `X-Forwarded-For`,
  so its log lines (`remote=…`) name the proxy, not the builder.
