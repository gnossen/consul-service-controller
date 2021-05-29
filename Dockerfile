FROM golang:1.16-stretch

COPY *.go go.mod go.sum /build/

WORKDIR /build

RUN go get -t .
RUN go build


FROM phusion/baseimage:18.04-1.0.0

COPY --from=0 /build/consul_service_controller /consul_service_controller

CMD consul_service_controller
