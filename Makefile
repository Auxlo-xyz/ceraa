build:
	go build -trimpath -o bin/ceraa .

test:
	go test -race ./...

check:
	go vet ./...
	gofmt -l cmd harness internal

fuzz:
	go test -run Fuzz -fuzz=FuzzInboxControlValidation -fuzztime=10s ./harness/coordinator/...
	go test -run Fuzz -fuzz=FuzzSessionStoreCorrupt -fuzztime=10s ./harness/coordinator/...
	go test -run Fuzz -fuzz=FuzzOperationSpec -fuzztime=10s ./harness/coordinator/...

clean:
	rm -rf bin/ testdata-fuzz/ sessions/

.PHONY: build test check fuzz clean
