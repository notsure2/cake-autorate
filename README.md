# cake-autorate
Golang port of https://github.com/lynxthecat/sqm-autorate

See original OpenWRT thread: https://forum.openwrt.org/t/cake-w-adaptive-bandwidth/108848

This version tries to use as little CPU as possible by not using any external processes and communicating with netlink directly with socket reuse.

