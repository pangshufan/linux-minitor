#!/usr/bin/env bash
# From a Linux Agent release: sudo bash install-agent.sh
set -euo pipefail
if [[ $(id -u) != 0 ]]; then
  echo "Please run as root: sudo bash install-agent.sh" >&2
  exit 1
fi
for tool in python3 systemctl; do
  command -v "$tool" >/dev/null || { echo "Required command not found: $tool" >&2; exit 1; }
done
package_dir=$(cd -- "$(dirname -- "$0")" && pwd)
python3 - "$package_dir" <<'PY'
import getpass
import json
import os
import platform
import re
import shutil
import ssl
import subprocess
import sys
import tempfile
import time
import zipfile
from pathlib import Path

sys.stdin = open("/dev/tty")
package = Path(sys.argv[1])
destination = Path("/opt/monitor")
config_dir = Path("/etc/monitor")
service = "monitor-agent.service"
unit_dir = Path("/etc/systemd/system")

def ask(label, default=""):
    return input(label + (f" [{default}]" if default else "") + ": ").strip() or default

def yes(label, default=True):
    while True:
        answer = ask(label + " (y/n)", "y" if default else "n").lower()
        if answer in ("y", "yes", "n", "no"):
            return answer in ("y", "yes")

def main():
    print("Monitor Agent interactive installer (Linux, systemd)")
    print("Install to /opt/monitor; configuration and certificates to /etc/monitor.")
    source = package / "bin/monitor-agent"
    if not source.is_file():
        raise ValueError("Missing bin/monitor-agent. Extract the complete Linux Agent release first.")
    if platform.system() != "Linux" or tuple(map(int, platform.release().split("-")[0].split(".")[:2])) < (4, 18):
        raise ValueError("Linux kernel 4.18 or newer is required.")
    with tempfile.TemporaryDirectory(prefix="monitor-install-") as tmp:
        temp = Path(tmp)
        cert_source = ask("Certificate ZIP, extracted certificate directory, or existing agent.json (Enter for manual/plaintext setup)")
        config, origin = {}, None
        if cert_source:
            candidate = Path(cert_source).expanduser().resolve()
            if candidate.is_file() and zipfile.is_zipfile(candidate):
                with zipfile.ZipFile(candidate) as bundle:
                    for name in ("agent.json", "agent.crt", "agent.key", "ca.crt"):
                        item = bundle.getinfo(name)
                        if item.file_size > 1024 * 1024:
                            raise ValueError("Certificate package file exceeds 1 MiB.")
                        (temp / name).write_bytes(bundle.read(name))
                origin = temp
                config = json.loads((temp / "agent.json").read_text(encoding="utf-8-sig"))
            else:
                cfg = candidate / "agent.json" if candidate.is_dir() else candidate
                origin = cfg.parent
                config = json.loads(cfg.read_text(encoding="utf-8-sig"))
        server = ask("Server gRPC address (host:port)", config.get("server", ""))
        if not re.fullmatch(r"(\[[^\]\s]+\]|[^:\s/]+):[0-9]+", server) or not 0 < int(server.rsplit(":", 1)[1]) <= 65535:
            raise ValueError("Invalid Server address; use host:port or [IPv6]:port.")
        host_id = ask("Unique host ID (must match certificate identity)", config.get("host-id", platform.node()))
        if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}", host_id):
            raise ValueError("Invalid host ID.")
        tls = yes("Enable mutual TLS", config.get("tls-enabled", True))
        config.update({"server": server, "host-id": host_id, "tls-enabled": tls})
        certs = {}
        if tls:
            for name, key in (("agent.crt", "tls-cert"), ("agent.key", "tls-key"), ("ca.crt", "tls-ca")):
                bundled = origin / name if origin else None
                default = str(bundled) if bundled and bundled.is_file() else config.get(key, "")
                path = Path(ask("Path to " + name, default)).expanduser().resolve()
                certs[name] = path.read_bytes()
                config[key] = str(config_dir / name)
            client = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
            client.load_verify_locations(cadata=certs["ca.crt"].decode())
            for name in ("agent.crt", "agent.key"):
                (temp / name).write_bytes(certs[name])
                (temp / name).chmod(0o600)
            client.load_cert_chain(str(temp / "agent.crt"), str(temp / "agent.key"))
        else:
            for key in ("tls-cert", "tls-key", "tls-ca"):
                config.pop(key, None)
        if yes("Set or replace the shared authentication token", False):
            config["auth-token"] = getpass.getpass("Agent token (input hidden): ")
        print("Existing process-services settings are preserved. Edit /etc/monitor/agent.json to change process monitoring.")
        print("Target:", server, "Host:", host_id, "TLS:", tls)
        if not yes("Install/update and start the Agent service"):
            print("Cancelled; installation unchanged.")
            return
        config_dir.mkdir(parents=True, exist_ok=True)
        (destination / "bin").mkdir(parents=True, exist_ok=True)
        backup = config_dir / ("backup-" + time.strftime("%Y%m%d-%H%M%S") + "-" + str(os.getpid()))
        backup.mkdir(mode=0o700)
        for name in ("agent.json", "agent.crt", "agent.key", "ca.crt"):
            old = config_dir / name
            if old.exists():
                shutil.copy2(old, backup / name)
                (backup / name).chmod(0o600)
        exe = destination / "bin/monitor-agent"
        unit = unit_dir / service
        for old, name in ((exe, "monitor-agent"), (unit, service)):
            if old.exists():
                shutil.copy2(old, backup / name)
        active = subprocess.run(["systemctl", "is-active", "--quiet", service]).returncode == 0
        if active:
            subprocess.run(["systemctl", "stop", service], check=True)
        new_exe = temp / "monitor-agent"
        shutil.copyfile(source, new_exe)
        shutil.copyfile(new_exe, str(exe) + ".new")
        os.chmod(str(exe) + ".new", 0o755)
        os.replace(str(exe) + ".new", exe)
        for name, data in certs.items():
            target = config_dir / name
            target.write_bytes(data)
            target.chmod(0o600)
        cfg = config_dir / "agent.json"
        cfg.write_text(json.dumps(config, indent=2) + "\n")
        cfg.chmod(0o600)
        unit.write_text("""[Unit]
Description=Monitor Agent
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/monitor
EnvironmentFile=-/etc/monitor/agent.env
ExecStart=/opt/monitor/bin/monitor-agent -config /etc/monitor/agent.json
Restart=on-failure
RestartSec=3s
UMask=0077
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
""")
        unit.chmod(0o644)
        subprocess.run(["systemctl", "daemon-reload"], check=True)
        subprocess.run(["systemctl", "enable", service], check=True)
        subprocess.run(["systemctl", "start", service], check=True)
        time.sleep(2)
        subprocess.run(["systemctl", "is-active", service], check=True)
        print("Installed. Backup:", backup)
        print("Service running does not prove Server registration; verify the host is online in Web.")
        print("Logs: journalctl -u monitor-agent.service -n 50 --no-pager")

try:
    main()
except (ValueError, OSError, KeyError, subprocess.CalledProcessError, zipfile.BadZipFile) as exc:
    print("Installation failed:", str(exc), file=sys.stderr)
    print("If updated, restore files from /etc/monitor/backup-* before restarting the service.", file=sys.stderr)
    sys.exit(1)
PY
