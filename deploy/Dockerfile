# nexthop 网关服务镜像（alpine，单二进制）。
# 容器内需要 NET_ADMIN（改路由）与 NET_RAW（ICMP ping）：
#   docker build -f deploy/Dockerfile -t nexthop .
#   docker run -d --name nexthop \
#     --network host --cap-add NET_ADMIN --cap-add NET_RAW \
#     -v /etc/nexthop/config.yaml:/etc/nexthop/config.yaml:ro \
#     nexthop
FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/nexthop ./cmd/nexthop

FROM alpine:3.23

RUN apk add --no-cache ca-certificates iproute2
COPY --from=build /out/nexthop /usr/sbin/nexthop
COPY cmd/nexthop/example.yaml /etc/nexthop/config.yaml

ENTRYPOINT ["/usr/sbin/nexthop"]
CMD ["run", "-c", "/etc/nexthop/config.yaml", "-s", "/run/nexthop.sock"]
