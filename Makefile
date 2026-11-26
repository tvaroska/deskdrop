VERSION ?= v1.4.3
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test vet build clean

vet:
	test -z "$$(gofmt -s -l .)"
	go vet ./...
	GOOS=windows GOARCH=amd64 go vet ./cmd/deskdrop ./internal/driver/...
	@if command -v ast-grep >/dev/null 2>&1; then ast-grep scan -r specs/ast.rules.yaml; fi

test: vet
	go test -race -count=1 ./...

build:
	mkdir -p dist/latest dist/$(VERSION)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/deskdrop ./cmd/deskdrop
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/deskdrop.exe ./cmd/deskdrop
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/deskdrop-server ./cmd/server
	cp dist/deskdrop dist/deskdrop.exe deskdrop.example.json dist/latest/
	cd dist/latest && sha256sum deskdrop deskdrop.exe > SHA256SUMS.txt
	cp dist/latest/SHA256SUMS.txt dist/SHA256SUMS.txt
	cp dist/latest/* dist/$(VERSION)/

clean:
	rm -rf dist bin
