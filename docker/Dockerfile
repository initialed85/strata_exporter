FROM ubuntu:26.04
WORKDIR /srv
COPY ./entrypoint /srv/entrypoint
STOPSIGNAL SIGTERM
CMD ["/srv/entrypoint"]
