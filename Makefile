.PHONY: build install daemon-install daemon-uninstall daemon-start daemon-stop daemon-logs link clean

DAEMON_PLIST := $(HOME)/Library/LaunchAgents/com.look.daemon.plist

build:
	pnpm build

# Install CLI + /look skill on this machine
install: build
	bash scripts/install-skill.sh

# Install the queue-processing daemon (launchd). Only needed on machines
# that should process screenshot tasks pushed from other Macs.
daemon-install: build
	bash scripts/install-daemon.sh

daemon-uninstall:
	launchctl unload $(DAEMON_PLIST) 2>/dev/null || true
	rm -f $(DAEMON_PLIST)

daemon-start:
	launchctl load $(DAEMON_PLIST)

daemon-stop:
	launchctl unload $(DAEMON_PLIST)

daemon-logs:
	tail -f ~/.look/logs/daemon.stdout.log

# Interactive LAN discovery + tunnel/push/pull
link:
	bash scripts/mac-link.sh

clean:
	rm -rf dist
