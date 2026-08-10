FROM alpine:3.21

RUN apk add --no-cache ca-certificates sqlite curl jq su-exec \
    && adduser -D -u 10001 yocache \
    && mkdir -p /var/lib/yocache \
    && chown yocache:yocache /var/lib/yocache

COPY yocache /usr/local/bin/yocache
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# image.source is what links the package to the repository on GHCR, which is
# how that page gets a README at all — there is no per-package README.
# image.description is set at build time from .github/registry/short-description.txt
# (see .goreleaser.yaml) so it stays in step with the Docker Hub page.
LABEL org.opencontainers.image.title="YoCache" \
      org.opencontainers.image.url="https://yocache.dev" \
      org.opencontainers.image.documentation="https://yocache.dev/docker/" \
      org.opencontainers.image.source="https://github.com/bmoczulski/yocache" \
      org.opencontainers.image.licenses="Apache-2.0"

EXPOSE 6768 6767
VOLUME ["/var/lib/yocache"]
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["--addr", ":6768", "--hashequiv-addr", ":6767", "--data-dir", "/var/lib/yocache"]
