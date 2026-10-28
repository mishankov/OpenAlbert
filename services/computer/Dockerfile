FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8
RUN apt-get update && apt-get install -y --no-install-recommends \
    openssh-server sudo python3 python3-venv python3-pip git curl ca-certificates \
    bash coreutils procps tini && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 --shell /bin/bash albert \
    && usermod --password '*' albert \
    && printf 'albert ALL=(ALL:ALL) NOPASSWD: ALL\n' > /etc/sudoers.d/albert \
    && chmod 0440 /etc/sudoers.d/albert && visudo -cf /etc/sudoers.d/albert \
    && mkdir -p /workspace /run/sshd && chown albert:albert /workspace \
    && rm -f /etc/ssh/ssh_host_*
COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /usr/local/bin/computer-entrypoint
RUN chmod 0755 /usr/local/bin/computer-entrypoint
WORKDIR /workspace
EXPOSE 22
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/computer-entrypoint"]
