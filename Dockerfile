FROM golang:1.8 AS build
COPY . /go/src/github.com/cpuguy83/nfs-rest-gateway
WORKDIR /go/src/github.com/cpuguy83/nfs-rest-gateway
RUN CGO_ENABLED=0 go build -o gateway

FROM alpine AS image
RUN apk add --no-cache nfs-utils curl vim
COPY --from=build /go/src/github.com/cpuguy83/nfs-rest-gateway/gateway /usr/bin/nfs-rest-gateway
VOLUME "/data"
ENTRYPOINT ["/usr/bin/nfs-rest-gateway"]
CMD ["--root=/data", "-H", "0.0.0.0:80"]
