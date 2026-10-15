FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mlw-proxy . && mkdir /state

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mlw-proxy /mlw-proxy
# writable state directory for the non-root user (nonroot = uid 65532)
COPY --from=build --chown=65532:65532 /state /var/lib/mlw-proxy
EXPOSE 8000 2222
ENTRYPOINT ["/mlw-proxy"]
