# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
# Override for restricted networks, e.g. --build-arg GOPROXY=https://goproxy.cn,direct
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV CGO_ENABLED=0 GOPROXY=${GOPROXY} GOSUMDB=${GOSUMDB}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/my_sync ./cmd/my_sync

FROM alpine:3.22
RUN apk add --no-cache tzdata ca-certificates
COPY --from=build /out/my_sync /usr/local/bin/my_sync
ENV MYSYNC_CONFIG=/config/config.yaml \
    TZ=Asia/Shanghai
VOLUME ["/config", "/backups"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["my_sync"]
CMD ["serve"]
