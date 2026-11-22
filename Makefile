VERSION ?= v1.3.1
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test vet build clean

vet:
	test -z "$$(gofmt -s -l .)"
	go vet ./...

test: vet
	go test -race ./...

build:
	mkdir -p dist/latest dist/$(VERSION)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/deskdrop ./cmd/deskdrop
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/deskdrop.exe ./cmd/deskdrop
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/deskdrop-server ./cmd/server
	cd dist && sha256sum deskdrop deskdrop.exe deskdrop-server > SHA256SUMS.txt
	cp dist/deskdrop dist/deskdrop.exe deskdrop.example.json dist/latest/
	cd dist/latest && sha256sum deskdrop deskdrop.exe deskdrop.example.json > SHA256SUMS.txt
	cp dist/latest/* dist/$(VERSION)/

clean:
	rm -rf dist bin
