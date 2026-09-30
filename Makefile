build:
	go build -trimpath -o bin/ceraa .

test:
	go test -race ./...

check:
	go vet ./...
	# "." is the main package that `build` compiles, so it belongs here. It was
	# omitted once and nothing caught it, because a formatting slip in main.go is
	# invisible to every other target.
	gofmt -l . cmd harness internal

fuzz:
	go test -run Fuzz -fuzz=FuzzInboxControlValidation -fuzztime=10s ./harness/coordinator/...
	go test -run Fuzz -fuzz=FuzzSessionStoreCorrupt -fuzztime=10s ./harness/coordinator/...
	go test -run Fuzz -fuzz=FuzzOperationSpec -fuzztime=10s ./harness/coordinator/...

clean:
	rm -rf bin/ testdata-fuzz/ sessions/

.PHONY: build test check fuzz clean
