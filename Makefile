.PHONY: build test install clean

build:
	go build -o roster .

test:
	go test ./... -v -count=1

install: build
	install -m 0755 roster $(HOME)/.local/bin/roster

clean:
	rm -f roster
