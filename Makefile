PACKAGES := ./cemi ./dpt ./apdu ./internal/util ./link/ft12 ./link/mock ./link/tpuart ./transport ./knxnet
# Packages whose tests do not fit the 256 KB flash of cortex-m-qemu run on riscv-qemu instead.
LARGE    := ./device
BOARDS   := pico esp32-coreboard-v2 xiao-esp32c3
BUILD    := build

.PHONY: check test tinygo-test tinygo-build clean

check: test tinygo-test tinygo-build

# Regular Go tests on the host.
test:
	go vet ./...
	go test -race ./...

# Run the unit tests on an emulated ARM Cortex-M and RISC-V (needs qemu-system-arm and
# qemu-system-riscv32).
tinygo-test:
	for p in $(PACKAGES); do tinygo test -target=cortex-m-qemu $$p || exit 1; done
	for p in $(LARGE); do tinygo test -target=riscv-qemu $$p || exit 1; done
	tinygo run -target=cortex-m-qemu ./examples/smoke

# Compile firmware for real boards to make sure the core stays TinyGo compatible.
tinygo-build:
	mkdir -p $(BUILD)
	for b in $(BOARDS); do tinygo build -target=$$b -o $(BUILD)/smoke-$$b.bin ./examples/smoke || exit 1; done
	tinygo build -target=pico -o $(BUILD)/pico-kberry.uf2 ./examples/pico-kberry

clean:
	rm -rf $(BUILD)
