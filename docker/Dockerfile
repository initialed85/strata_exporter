FROM alpine:3.21
RUN addgroup -S exporter && adduser -S -G exporter exporter
WORKDIR /srv
COPY ./entrypoint /srv/entrypoint
USER exporter
STOPSIGNAL SIGTERM
CMD ["/srv/entrypoint"]
