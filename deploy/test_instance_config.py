"""Render deployment config without starting services or printing credentials.

Run: python3 deploy/test_instance_config.py
Requires Docker Compose and Helm (or the pinned Helm container), plus PyYAML.
"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parent.parent
INSTANCE = "11111111-2222-4333-8444-555555555555"
COMPONENTS = ("api", "agent-runner", "realtime")


class InstanceConfigTest(unittest.TestCase):
    def test_compose(self):
        with tempfile.NamedTemporaryFile(mode="w") as env_file:
            env_file.write("JWT_SECRET=synthetic-test\nADMIN_PASSWORD=synthetic-test\nINTERNAL_API_KEY=synthetic-test\n")
            env_file.flush()
            for mode in ("dev", "prod", "e2e"):
                cmd = ["docker", "compose", "--env-file", env_file.name, "-f", str(ROOT / f"deploy/docker-compose.{mode}.yml"), "config", "--format", "json"]
                for value in (INSTANCE, "66666666-7777-4888-8999-aaaaaaaaaaaa", "", None):
                    with self.subTest(mode=mode, instance=value):
                        env = dict(os.environ)
                        env.pop("PACA_INSTANCE_ID", None)
                        if value is not None:
                            env["PACA_INSTANCE_ID"] = value
                        result = subprocess.run(cmd, env=env, capture_output=True, text=True)
                        if not value:
                            self.assertNotEqual(result.returncode, 0, "missing ID must fail before starting services")
                            self.assertIn("PACA_INSTANCE_ID", result.stderr)
                            continue
                        self.assertEqual(result.returncode, 0, "valid config must render")
                        services = json.loads(result.stdout)["services"]
                        for component in COMPONENTS:
                            self.assertEqual(services[component]["environment"].get("PACA_INSTANCE_ID"), value, component)
                        self.assertEqual(services["realtime"]["environment"].get("INTERNAL_API_KEY"), services["api"]["environment"]["AI_AGENT_INTERNAL_KEY"])
                        self.assertEqual(services["agent-runner"]["environment"]["INTERNAL_API_KEY"], services["api"]["environment"]["AI_AGENT_INTERNAL_KEY"])

    def test_helm(self):
        helm = ["helm"] if shutil.which("helm") else ["docker", "run", "--rm", "-v", f"{ROOT}:/workspace:ro", "-w", "/workspace", "alpine/helm:3.17.3"]
        chart = str(ROOT / "deploy/helm/paca") if len(helm) == 1 else "deploy/helm/paca"
        for value, enabled in [(v, True) for v in (INSTANCE, "66666666-7777-4888-8999-aaaaaaaaaaaa", "", "not-a-uuid", "00000000-0000-0000-0000-000000000000")] + [(INSTANCE, False)]:
            with self.subTest(instance=value, runner=enabled):
                settings = {f"secrets.{key}": "synthetic-test" for key in ("jwtSecret", "adminPassword", "internalApiKey", "agentApiKey", "postgresPassword", "storageAccessKeyId", "storageSecretAccessKey")}
                settings["secrets.encryptionKey"] = "a" * 64
                settings["instanceId"] = value
                cmd = helm + ["template", "instance-check", chart, "--set", f"agentRunner.enabled={str(enabled).lower()}", "--set-string", ",".join(f"{k}={v}" for k, v in settings.items())]
                result = subprocess.run(cmd, capture_output=True, text=True)
                if value in ("", "not-a-uuid", "00000000-0000-0000-0000-000000000000"):
                    self.assertNotEqual(result.returncode, 0, "invalid ID must fail Helm rendering")
                    self.assertIn("instanceId", result.stderr)
                    continue
                self.assertEqual(result.returncode, 0, "valid config must render")
                components = {}
                secret = {}
                for doc in yaml.safe_load_all(result.stdout):
                    if doc and doc.get("kind") == "Secret":
                        secret = doc["stringData"]
                    if doc and doc.get("kind") == "Deployment":
                        for container in doc["spec"]["template"]["spec"]["containers"]:
                            if container["name"] in COMPONENTS:
                                components[container["name"]] = {item["name"]: item for item in container["env"]}
                self.assertEqual(set(components), set(COMPONENTS if enabled else ("api", "realtime")))
                self.assertTrue("INTERNAL_API_KEY" in secret, "API and realtime need the existing key even when runner is disabled")
                self.assertIn("AI_AGENT_INTERNAL_KEY", components["api"])
                for component, env in components.items():
                    self.assertEqual(env.get("PACA_INSTANCE_ID", {}).get("value"), value, component)
                for component in (("agent-runner", "realtime") if enabled else ("realtime",)):
                    self.assertEqual(components[component].get("INTERNAL_API_KEY", {}).get("valueFrom"), components["api"]["AI_AGENT_INTERNAL_KEY"]["valueFrom"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
