.PHONY: all frontend server hostrctl test clean docker run

all: frontend server hostrctl

# Builds the SvelteKit SPA and drops it where go:embed picks it up.
# placeholder.html is left alone so a Node-less build still has something to serve.
frontend:
	cd frontend && npm ci && npm run build
	find web -mindepth 1 ! -name placeholder.html -delete
	cp -R frontend/build/. web/

server:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/hostr .

hostrctl:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/hostrctl ./cmd/hostrctl

test:
	go test ./...

# Local run: plain HTTP, control panel on any hostname, data in ./data.
run: server
	HOSTR_DATA=./data HOSTR_ADDR=:8080 HOSTR_SECURE_COOKIES=false ./bin/hostr

docker:
	docker build -t hostr:latest .

clean:
	rm -rf bin frontend/build frontend/.svelte-kit
	find web -mindepth 1 ! -name placeholder.html -delete
