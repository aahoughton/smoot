# syntax=docker/dockerfile:1

FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/smoot ./cmd/smoot
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/smoot /usr/local/bin/smoot
COPY --from=build --chown=nonroot:nonroot /out/data /data
# Published ports are reached as localhost; set SMOOT_ALLOWED_HOSTS to the
# names clients use (or *) when exposing the server beyond this machine.
ENV SMOOT_DB=/data/smoot.db \
    SMOOT_ADDR=:8787 \
    SMOOT_ALLOWED_HOSTS=localhost,127.0.0.1,::1
VOLUME ["/data"]
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
    CMD ["smoot", "health", "--url", "http://127.0.0.1:8787"]
ENTRYPOINT ["smoot"]
CMD ["serve"]
