plugin:=nomad-driver-wasm
.PHONY: all
all: build

build: $(plugin)
$(plugin):
	@go build

.PHONY: clean
clean:
	@rm -f $(plugin)
