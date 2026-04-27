.PHONY: install run run-daemon build pull daemon-stop daemon-restart daemon-status daemon-log

INSTALL_DIR := $(HOME)/.local/bin
LAUNCHD_LABEL := com.xmuggle.daemon

install: pull build
	npm install
	install -d $(INSTALL_DIR)
	install -m 0755 xmuggled $(INSTALL_DIR)/xmuggled
	launchctl kill SIGTERM gui/$(shell id -u)/$(LAUNCHD_LABEL) 2>/dev/null || true

pull:
	git pull --rebase

run:
	npm start

run-daemon:
	$(INSTALL_DIR)/xmuggled start

build:
	go build -o xmuggled ./cmd/xmuggled/

daemon-stop:
	$(INSTALL_DIR)/xmuggled stop

daemon-restart:
	-$(INSTALL_DIR)/xmuggled stop 2>/dev/null
	-rm -f $(HOME)/.xmuggle/daemon.pid
	$(INSTALL_DIR)/xmuggled start

daemon-status:
	$(INSTALL_DIR)/xmuggled status

log:
	tail -f $(HOME)/.xmuggle/daemon.log
