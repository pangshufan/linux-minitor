"""Exercise the installer in a sandbox, with fake systemd calls (no root required)."""
import io
import json
import os
import subprocess
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest.mock import patch


class LinuxInstallerTests(unittest.TestCase):
    def load_installer(self, root):
        script = Path(__file__).resolve().parents[1] / "linux/install-agent.sh"
        code = script.read_text().split("<<'PY'\n", 1)[1].rsplit("\nPY", 1)[0]
        code = code.replace('sys.stdin = open("/dev/tty")', '')
        code = code[:code.rindex("\ntry:\n    main()")]
        package = root / "package"
        (package / "bin").mkdir(parents=True)
        (package / "bin/monitor-agent").write_text("new executable")
        namespace = {}
        with patch("sys.argv", ["installer", str(package)]):
            exec(compile(code, str(script), "exec"), namespace)
        namespace.update({
            "destination": root / "opt/monitor",
            "config_dir": root / "etc/monitor",
            "unit_dir": root / "systemd",
        })
        namespace["unit_dir"].mkdir()
        return namespace

    def execute(self, ns, answers):
        commands = []
        def systemctl(args, **kwargs):
            commands.append(args)
            return subprocess.CompletedProcess(args, 0)
        with patch("builtins.input", side_effect=answers), patch("platform.system", return_value="Linux"), \
             patch("platform.release", return_value="5.15.0"), patch("subprocess.run", side_effect=systemctl), \
             patch("time.sleep"), patch("sys.stdout", new_callable=io.StringIO):
            ns["main"]()
        return commands

    def test_plaintext_update_backs_up_and_preserves_selectors(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            ns = self.load_installer(root)
            old = ns["config_dir"]
            old.mkdir(parents=True)
            selectors = [{"name": "nginx", "match": {"executable": "/usr/sbin/nginx"}}]
            original = json.dumps({"server": "old:8443", "host-id": "node",
                                   "tls-enabled": False, "process-services": selectors})
            config = old / "agent.json"
            config.write_text(original)
            commands = self.execute(ns, [str(config), "new:8443", "", "n", "n", "y"])
            result = json.loads(config.read_text())
            self.assertEqual(result["server"], "new:8443")
            self.assertEqual(result["process-services"], selectors)
            self.assertEqual(config.stat().st_mode & 0o777, 0o600)
            backup = list(old.glob("backup-*"))[0]
            self.assertEqual((backup / "agent.json").read_text(), original)
            unit = (ns["unit_dir"] / "monitor-agent.service").read_text()
            self.assertIn("ExecStart=/opt/monitor/bin/monitor-agent -config /etc/monitor/agent.json", unit)
            self.assertIn(["systemctl", "stop", "monitor-agent.service"], commands)
            self.assertIn(["systemctl", "start", "monitor-agent.service"], commands)

    def test_cancel_leaves_installation_untouched(self):
        with tempfile.TemporaryDirectory() as temp:
            ns = self.load_installer(Path(temp))
            commands = self.execute(ns, ["", "monitor:8443", "node", "n", "n", "n"])
            self.assertFalse(ns["config_dir"].exists())
            self.assertFalse(commands)

    def test_zip_import_rewrites_windows_paths(self):
        # Real SSL parsing catches a broken key/certificate pair before installation.
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            ns = self.load_installer(root)
            key, cert = root / "key.pem", root / "cert.pem"
            subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                            "-subj", "/CN=node", "-days", "1", "-keyout", str(key), "-out", str(cert)],
                           check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            bundle = root / "certificates.zip"
            with zipfile.ZipFile(bundle, "w") as z:
                z.writestr("agent.json", json.dumps({"server": "monitor:8443", "host-id": "node",
                    "tls-enabled": True, "tls-cert": "C:/ProgramData/Monitor/agent.crt"}))
                for name, path in (("agent.crt", cert), ("agent.key", key), ("ca.crt", cert)):
                    z.write(path, name)
                z.writestr("../outside", "must not extract")
            self.execute(ns, [str(bundle), "", "", "", "", "", "", "n", "y"])
            config = json.loads((ns["config_dir"] / "agent.json").read_text())
            self.assertEqual(config["tls-cert"], str(ns["config_dir"] / "agent.crt"))
            self.assertEqual((ns["config_dir"] / "agent.key").stat().st_mode & 0o777, 0o600)
            self.assertFalse((root / "outside").exists())


if __name__ == "__main__":
    unittest.main()
