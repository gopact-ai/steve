# The machine an acceptance node runs on. It holds no Steve build: the
# suite copies the binaries it just compiled into a running container, so
# this image stays valid across code changes and is built once per machine.
#
# What is installed is what the scenarios reach for on a node: git for the
# shadow repositories and worktrees, procps so a test can prove an agent
# process really is here, curl to call the reverse MCP endpoint from the
# node's own loopback, and ca-certificates for anything that leaves.
FROM debian:stable-slim

RUN apt-get -qq update \
    && apt-get -qq install --no-install-recommends -y ca-certificates curl git procps \
    && rm -rf /var/lib/apt/lists/*

# An acceptance node is not root, the way a real one is not.
RUN useradd --create-home --shell /bin/bash steve
COPY nodectl /usr/local/bin/nodectl
RUN chmod 0755 /usr/local/bin/nodectl

USER steve
WORKDIR /home/steve
RUN mkdir -p /home/steve/steve-bin /home/steve/steve-work /home/steve/steve-state

# Git refuses to work in a directory it considers someone else's, and
# refuses to commit without an identity; a node does both on its own.
RUN git config --global user.name "Steve Node" \
    && git config --global user.email "node@steve.invalid" \
    && git config --global init.defaultBranch main \
    && git config --global --add safe.directory '*'

# The container outlives the node process: scenarios stop and start
# steve-node through nodectl and expect the machine to still be there.
CMD ["sleep", "infinity"]
