all: vet test testrace testappengine

build: deps
	go build github.com/AmirSoleimani/grpc-go/...

clean:
	go clean -i github.com/AmirSoleimani/grpc-go/...

deps:
	go get -d -v github.com/AmirSoleimani/grpc-go/...

proto:
	@ if ! which protoc > /dev/null; then \
		echo "error: protoc not installed" >&2; \
		exit 1; \
	fi
	go generate github.com/AmirSoleimani/grpc-go/...

test: testdeps
	go test -cpu 1,4 -timeout 7m github.com/AmirSoleimani/grpc-go/...

testappengine: testappenginedeps
	goapp test -cpu 1,4 -timeout 7m github.com/AmirSoleimani/grpc-go/...

testappenginedeps:
	goapp get -d -v -t -tags 'appengine appenginevm' github.com/AmirSoleimani/grpc-go/...

testdeps:
	go get -d -v -t github.com/AmirSoleimani/grpc-go/...

testrace: testdeps
	go test -race -cpu 1,4 -timeout 7m github.com/AmirSoleimani/grpc-go/...

updatedeps:
	go get -d -v -u -f github.com/AmirSoleimani/grpc-go/...

updatetestdeps:
	go get -d -v -t -u -f github.com/AmirSoleimani/grpc-go/...

vet: vetdeps
	./vet.sh

vetdeps:
	./vet.sh -install

.PHONY: \
	all \
	build \
	clean \
	deps \
	proto \
	test \
	testappengine \
	testappenginedeps \
	testdeps \
	testrace \
	updatedeps \
	updatetestdeps \
	vet \
	vetdeps
