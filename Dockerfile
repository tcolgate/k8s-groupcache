FROM golang:1.10

COPY . /go/src/github.com/tcolgate/k8s-groupcache
RUN go get github.com/tcolgate/k8s-groupcache
RUN go install github.com/tcolgate/k8s-groupcache

ENTRYPOINT ["/go/bin/k8s-groupcache"]
