FROM debian:bookworm-slim
RUN apt-get update     && apt-get install -y --no-install-recommends ca-certificates util-linux     && rm -rf /var/lib/apt/lists/*
COPY thunder-dra-operator /usr/local/bin/thunder-dra-operator
ENTRYPOINT ["/usr/local/bin/thunder-dra-operator"]
