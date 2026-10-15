FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mlw-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mlw-proxy /mlw-proxy
EXPOSE 8000 2222
ENTRYPOINT ["/mlw-proxy"]
