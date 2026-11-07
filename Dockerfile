# Built by GoReleaser from the release binaries; see .goreleaser.yaml.
FROM --platform=$BUILDPLATFORM busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec AS dirs
RUN mkdir -p /var/lib/zerobill

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
ARG TARGETPLATFORM
COPY --from=dirs --chown=65532:65532 /var/lib/zerobill /var/lib/zerobill
COPY $TARGETPLATFORM/zerobill /usr/bin/zerobill
# Mount the config at /etc/zerobill/config.yaml and keep /var/lib/zerobill on a volume.
ENV XDG_CONFIG_HOME=/etc XDG_STATE_HOME=/var/lib
ENTRYPOINT ["/usr/bin/zerobill"]
CMD ["run"]
