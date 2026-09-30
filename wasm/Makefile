.PHONY: build run clean

build:
	mkdir -p build
	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -o build/guest.wasm ./guest
	go build -o build/host ./host

run: build
	./build/host ./build/guest.wasm add 20 22

clean:
	rm -rf build
