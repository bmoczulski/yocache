FROM alpine:3.21

RUN apk add --no-cache ca-certificates sqlite curl jq su-exec \
    && adduser -D -u 10001 yocache \
    && mkdir -p /var/lib/yocache \
    && chown yocache:yocache /var/lib/yocache

COPY yocache /usr/local/bin/yocache
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

EXPOSE 6768 6767
VOLUME ["/var/lib/yocache"]
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["--addr", ":6768", "--hashequiv-addr", ":6767", "--data-dir", "/var/lib/yocache"]
