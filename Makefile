APP := gasoline
DIST_DIR := dist
BINDIR ?= /usr/local/bin
WEB_INSTALL_DIR ?= /var/www/html/$(APP)

.PHONY: build test tidy fmt clean install release

build:
	go build -o $(APP) .

test:
	go test ./...
	bash gasoline-watch_test.sh
	@if locale -a | grep -qx 'de_DE.utf8'; then LC_ALL=de_DE.utf8 bash gasoline-watch_test.sh; else echo "skipping de_DE.utf8 watcher test"; fi

tidy:
	go mod tidy

fmt:
	gofmt -w *.go

clean:
	rm -rf $(APP) $(DIST_DIR)

install: build
	install -d $(BINDIR)
	install -m 0755 $(APP) $(BINDIR)/$(APP)
	install -d $(WEB_INSTALL_DIR)
	cp -R web/. $(WEB_INSTALL_DIR)/

release:
	mkdir -p $(DIST_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(DIST_DIR)/$(APP)-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o $(DIST_DIR)/$(APP)-linux-arm64 .
	GOOS=linux GOARCH=arm GOARM=7 go build -o $(DIST_DIR)/$(APP)-linux-armv7 .
