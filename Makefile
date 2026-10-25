BINARY := shakedown

.PHONY: build test fmt vet check cover clean demo

build:
	go build -o $(BINARY) ./cmd/shakedown

test:
	go test -race ./...

fmt:
	gofmt -l -w .

vet:
	go vet ./...

check: fmt vet test

cover:
	go test -cover ./...

clean:
	rm -f $(BINARY)

# Start a flaky API, hit it, then shut the API down.
demo: build
	./$(BINARY) mock --profile flaky --listen 127.0.0.1:9090 & \
	sleep 1; \
	./$(BINARY) run --target http://127.0.0.1:9090/orders -c 50 -d 10s --timeout 2s; \
	kill %1
