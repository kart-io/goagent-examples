unexport GOROOT
export GOWORK = off

.PHONY: demo test build

demo:
	go run . demo -auto

test:
	go test -race -count=1 -timeout=60s ./...

build:
	go build -o /tmp/goagent-protocol-agent .
