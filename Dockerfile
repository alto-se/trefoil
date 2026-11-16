# trefoil API server image.
#
# The server builds and runs agents by calling the docker CLI, so mount the
# host's Docker socket and keep the data dir on a volume:
#
#   docker run -p 8080:8080 \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     -v trefoil-data:/data \
#     ghcr.io/alto-se/trefoil-api
#
# Mount a backend.yml and set TREFOIL_SETTINGS to configure it further;
# ADDR, DATA_DIR and MODULES_DIR still override it.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY api/go.mod api/go.sum ./
RUN go mod download
COPY api/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM docker:27-cli
COPY --from=build /out/api /usr/local/bin/trefoil-api
COPY modules/ /trefoil/modules/
ENV ADDR=:8080 \
    DATA_DIR=/data \
    MODULES_DIR=/trefoil/modules
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["trefoil-api"]
