# Curio build entrypoint.
#
# The project Makefile is intentionally split into focused include files under
# scripts/makefiles/ to keep behavior coherent and maintainable.
#
# See documentation/en/developer/make-variables.md for variable/override guidance.

include scripts/makefiles/00-vars.mk
include scripts/makefiles/05-help.mk
include scripts/makefiles/10-deps.mk
include scripts/makefiles/20-test.mk
include scripts/makefiles/30-build.mk
include scripts/makefiles/40-gen.mk
include scripts/makefiles/50-docker.mk
include scripts/makefiles/60-abi.mk

## CUZK PROVING DAEMON (Rust, requires CUDA)
CUZK_PATH := extern/cuzk
CUZK_BIN := $(CUZK_PATH)/target/release/cuzk-daemon
.PHONY: cuzk
cuzk:
	@if ! command -v cargo >/dev/null 2>&1; then \
		echo ""; \
		echo "ERROR: cargo (Rust) not found. cuzk requires the Rust toolchain."; \
		echo "Install from https://rustup.rs/"; \
		echo ""; \
		exit 1; \
	fi
	@if ! command -v nvcc >/dev/null 2>&1; then \
		echo ""; \
		echo "ERROR: nvcc not found. cuzk requires the CUDA toolkit."; \
		echo "Install the CUDA toolkit and ensure nvcc is in PATH."; \
		echo ""; \
		exit 1; \
	fi
	cd $(CUZK_PATH) && cargo build --release --bin cuzk-daemon
	cp $(CUZK_BIN) ./cuzk
install-cuzk:
	install -C ./cuzk /usr/local/bin/cuzk
.PHONY: install-cuzk
uninstall-cuzk:
	rm -f /usr/local/bin/cuzk
.PHONY: uninstall-cuzk
