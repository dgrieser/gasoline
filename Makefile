APP := gasoline
DIST_DIR := dist

.PHONY: build test tidy fmt clean release

build:
	go build -o $(APP) .

test:
	go test ./...

tidy:
	go mod tidy

fmt:
	gofmt -w *.go

clean:
	rm -rf $(APP) $(DIST_DIR)

release:
	mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(DIST_DIR)/$(APP)-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o $(DIST_DIR)/$(APP)-linux-arm64 .
	GOOS=linux GOARCH=arm GOARM=7 go build -o $(DIST_DIR)/$(APP)-linux-armv7 .
