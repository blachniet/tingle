FROM golang:1.6-alpine

COPY . $GOPATH/src/github.com/blachniet/tingle
WORKDIR $GOPATH/src/github.com/blachniet/tingle

RUN go install $(go list ./... | grep -v /vendor/)

ENTRYPOINT ["tingle"]