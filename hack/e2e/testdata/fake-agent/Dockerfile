# A fake agent image for the assistant's end-to-end tests: busybox, the
# static icaro binary (its MCP bridge) and a fake `claude`.
FROM mirror.gcr.io/library/busybox:1.36
COPY icaro /usr/local/bin/icaro
COPY claude /usr/local/bin/claude
