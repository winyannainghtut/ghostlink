"""What the client and mesh package scripts do to their service across install, upgrade and removal.

Release CI installs only the .deb, so the scripts run here in a POSIX shell against a stand-in
systemctl and a temporary root, following dpkg's and rpm's order of scripts.

Further down, the same is done for the server's host script (packages/linux/server/ghostlink-server.sh),
the installers (scripts/install-server.sh, scripts/install-mesh.sh) and the Linux build scripts, against
stand-ins for Docker, the firewall, swap and nfpm. What needs symlinks, FIFOs or a dd that opens with
O_NOFOLLOW runs on Linux only; the real thing, as root on a runner, is in .github/workflows/linux-package.yml.
"""
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
# Debian runs maintainer scripts with dash; prefer it where it is installed.
SHELL = shutil.which("dash") or shutil.which("sh")

SYSTEMCTL = """#!/bin/sh
# Stand-in for systemctl: records each call and keeps each unit's state as marker files.
echo "$*" >> "$FAKE_SYSTEMD/calls"
command= unit= now=
for argument in "$@"; do
    case $argument in
        --now) now=1 ;;
        -*) ;;
        *) if [ -z "$command" ]; then command=$argument; else unit=$argument; fi ;;
    esac
done
case $command in
    daemon-reload) ;;
    is-enabled) [ -e "$FAKE_SYSTEMD/$unit.enabled" ] ;;
    is-active) [ -e "$FAKE_SYSTEMD/$unit.active" ] ;;
    enable) : > "$FAKE_SYSTEMD/$unit.enabled"; [ -z "$now" ] || : > "$FAKE_SYSTEMD/$unit.active" ;;
    disable) rm -f "$FAKE_SYSTEMD/$unit.enabled"; [ -z "$now" ] || rm -f "$FAKE_SYSTEMD/$unit.active" ;;
    start | restart) : > "$FAKE_SYSTEMD/$unit.active" ;;
    stop) rm -f "$FAKE_SYSTEMD/$unit.active" ;;
    *) echo "systemctl stand-in: $command is not supported" >&2; exit 1 ;;
esac
"""

CLIENT = "ghostlinkd.service"
MESH = "ghostlink-mesh.service"
POSTINST = "packages/linux/scripts/client-postinstall.sh"
PRERM = "packages/linux/scripts/client-preremove.sh"
POSTRM = "packages/linux/scripts/client-postremove.sh"
POSTTRANS = "packages/linux/scripts/client-posttrans.sh"
MESH_POSTINST = "packages/linux/mesh/postinstall.sh"
MESH_PRERM = "packages/linux/mesh/preremove.sh"
MESH_POSTTRANS = "packages/linux/mesh/posttrans.sh"
# The settings file the client package installs, as scripts/build-linux.sh writes it.
TEMPLATE = re.search(r"cat > \"\$pkg/ghostlinkd\.env\" <<'EOF'\n(.*?\n)EOF\n",
                     (ROOT / "scripts/build-linux.sh").read_text(encoding="utf-8"), re.S).group(1)
OWNED = TEMPLATE.replace("GHOSTLINK_OWNER_UID=\n", "GHOSTLINK_OWNER_UID=1000\n")


@unittest.skipUnless(SHELL, "a POSIX shell is needed to run the Linux package scripts")
class LinuxPackageScripts(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.state = self.root / "systemd-state"
        for directory in ("bin", "etc/ghostlink", "run/systemd/system", "var/lib"):
            (self.root / directory).mkdir(parents=True)
        for name, text in (("systemctl", SYSTEMCTL), ("modprobe", "#!/bin/sh\nexit 0\n")):
            (self.root / "bin" / name).write_bytes(text.encode())
            (self.root / "bin" / name).chmod(0o755)
        self.env_file = self.root / "etc/ghostlink/ghostlinkd.env"
        self.reset(TEMPLATE)

    def reset(self, settings, unit=CLIENT, enabled=False, active=False):
        shutil.rmtree(self.state, ignore_errors=True)
        self.state.mkdir()
        self.env_file.write_bytes(settings.encode())
        for flag, wanted in (("enabled", enabled), ("active", active)):
            if wanted:
                (self.state / f"{unit}.{flag}").write_bytes(b"")

    def run_script(self, path, *arguments, sudo_uid=None):
        text = (ROOT / path).read_text(encoding="utf-8")
        for system in ("/etc/ghostlink", "/var/lib/ghostlink", "/run/systemd/system"):
            text = text.replace(system, self.root.as_posix() + system)
        script = self.root / Path(path).name
        script.write_bytes(text.encode())
        environment = {name: value for name, value in os.environ.items() if name != "SUDO_UID"}
        environment["PATH"] = str(self.root / "bin") + os.pathsep + environment.get("PATH", "")
        environment["FAKE_SYSTEMD"] = self.state.as_posix()
        if sudo_uid is not None:
            environment["SUDO_UID"] = sudo_uid
        result = subprocess.run([SHELL, script.as_posix(), *arguments], env=environment, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout + result.stderr

    def is_on(self, unit, flag):
        return (self.state / f"{unit}.{flag}").exists()

    def calls(self):
        log = self.state / "calls"
        return [line.split()[0] for line in log.read_text().splitlines()] if log.exists() else []

    def settings(self):
        return self.env_file.read_bytes().decode()

    def test_a_first_install_takes_the_sudo_user_and_starts_the_service(self):
        for arguments in (("configure", ""), ("1",)):
            with self.subTest(arguments=arguments):
                self.reset(TEMPLATE)
                output = self.run_script(POSTINST, *arguments, sudo_uid="1000")
                self.assertEqual(self.settings(), OWNED)
                self.assertTrue(self.is_on(CLIENT, "enabled") and self.is_on(CLIENT, "active"))
                self.assertIn("installed for user ID 1000", output)

    def test_a_first_install_without_an_owner_leaves_the_service_off(self):
        for sudo_uid in (None, "0"):
            with self.subTest(sudo_uid=sudo_uid):
                self.reset(TEMPLATE)
                output = self.run_script(POSTINST, "configure", "", sudo_uid=sudo_uid)
                self.assertEqual(self.settings(), TEMPLATE)
                self.assertFalse(self.is_on(CLIENT, "enabled") or self.is_on(CLIENT, "active"))
                self.assertIn("Set GHOSTLINK_OWNER_UID", output)

    def test_an_owner_written_in_another_form_systemd_accepts_is_kept(self):
        for line in ('GHOSTLINK_OWNER_UID="1000"', "GHOSTLINK_OWNER_UID='1000'", "GHOSTLINK_OWNER_UID = 1000",
                     "  GHOSTLINK_OWNER_UID=1000\t ", "GHOSTLINK_OWNER_UID=1000\r"):
            with self.subTest(line=line):
                settings = TEMPLATE.replace("GHOSTLINK_OWNER_UID=\n", line + "\n")
                self.reset(settings)
                output = self.run_script(POSTINST, "configure", "", sudo_uid="1001")
                self.assertEqual(self.settings(), settings)
                self.assertIn("installed for user ID 1000", output)
                self.assertTrue(self.is_on(CLIENT, "active"))

    def test_the_last_assignment_counts(self):
        self.reset(TEMPLATE.replace("GHOSTLINK_OWNER_UID=\n", "GHOSTLINK_OWNER_UID=1000\nGHOSTLINK_OWNER_UID=\n"))
        output = self.run_script(POSTINST, "configure", "", sudo_uid="1001")
        self.assertNotIn("GHOSTLINK_OWNER_UID=1000", self.settings())
        self.assertIn("installed for user ID 1001", output)

    def test_an_owner_that_is_not_a_user_id_is_left_alone(self):
        settings = TEMPLATE.replace("GHOSTLINK_OWNER_UID=\n", "GHOSTLINK_OWNER_UID=alice\n")
        self.reset(settings)
        output = self.run_script(POSTINST, "configure", "", sudo_uid="1001")
        self.assertEqual(self.settings(), settings)
        self.assertFalse(self.is_on(CLIENT, "enabled") or self.is_on(CLIENT, "active"))
        self.assertIn("is not a user ID", output)

    def test_a_debian_upgrade_restarts_an_enabled_service(self):
        self.reset(OWNED, enabled=True, active=True)
        self.run_script(PRERM, "upgrade", "0.8.1")
        self.assertFalse(self.is_on(CLIENT, "active"))
        self.run_script(POSTINST, "configure", "0.8.0", sudo_uid="1001")
        self.assertTrue(self.is_on(CLIENT, "enabled") and self.is_on(CLIENT, "active"))
        self.assertEqual(self.settings(), OWNED)

    def test_an_upgrade_leaves_a_disabled_service_off(self):
        for arguments in (("configure", "0.8.0"), ("2",)):
            with self.subTest(arguments=arguments):
                self.reset(OWNED)
                output = self.run_script(POSTINST, *arguments, sudo_uid="1000")
                self.assertFalse(self.is_on(CLIENT, "enabled") or self.is_on(CLIENT, "active"))
                self.assertFalse({"enable", "start", "restart"} & set(self.calls()))
                self.assertIn("service is disabled", output)

    def test_installing_again_after_a_removal_turns_the_service_back_on(self):
        self.reset(OWNED, enabled=True, active=True)
        self.run_script(PRERM, "remove")
        self.run_script(POSTRM, "remove")
        self.assertFalse(self.is_on(CLIENT, "enabled") or self.is_on(CLIENT, "active"))
        # Debian installs from the "config-files" state with the last configured version.
        self.run_script(POSTINST, "configure", "0.8.0")
        self.assertTrue(self.is_on(CLIENT, "enabled") and self.is_on(CLIENT, "active"))
        self.assertFalse((self.root / "var/lib/ghostlink/enable-on-install").exists())

    def test_a_removal_goes_ahead_when_the_state_directory_cannot_be_written(self):
        self.reset(OWNED, enabled=True, active=True)
        (self.root / "var/lib").rmdir()
        (self.root / "var/lib").write_bytes(b"")
        self.run_script(PRERM, "remove")
        self.assertFalse(self.is_on(CLIENT, "enabled") or self.is_on(CLIENT, "active"))

    def test_installing_again_after_removing_a_disabled_service_keeps_it_off(self):
        self.reset(OWNED)
        self.run_script(PRERM, "remove")
        self.run_script(POSTRM, "remove")
        self.run_script(POSTINST, "configure", "0.8.0")
        self.assertFalse(self.is_on(CLIENT, "enabled") or self.is_on(CLIENT, "active"))

    def test_an_rpm_upgrade_leaves_the_service_running(self):
        # rpm runs the new %post, then the old %preun, then %posttrans.
        self.reset(OWNED, enabled=True, active=True)
        self.run_script(POSTINST, "2")
        self.run_script(PRERM, "1")
        self.run_script(POSTTRANS)
        self.assertTrue(self.is_on(CLIENT, "enabled") and self.is_on(CLIENT, "active"))
        self.assertNotIn("stop", self.calls())

    def test_posttrans_starts_a_service_an_older_preremove_stopped(self):
        self.reset(OWNED, enabled=True, active=True)
        self.run_script(POSTINST, "2")
        # The pre-remove of packages up to 0.8.0 stops the service when rpm passes 1.
        (self.state / f"{CLIENT}.active").unlink()
        self.run_script(POSTTRANS)
        self.assertTrue(self.is_on(CLIENT, "active"))
        self.reset(OWNED)
        self.run_script(POSTTRANS)
        self.assertFalse(self.is_on(CLIENT, "active"))

    def test_a_mesh_rpm_upgrade_leaves_the_agent_running(self):
        node = self.root / "var/lib/ghostlink-mesh/node.json"
        node.parent.mkdir(parents=True)
        node.write_bytes(b"{}")
        self.reset(OWNED, unit=MESH, enabled=True, active=True)
        self.run_script(MESH_POSTINST, "2")
        self.run_script(MESH_PRERM, "1")
        self.assertTrue(self.is_on(MESH, "active"))
        (self.state / f"{MESH}.active").unlink()
        self.run_script(MESH_POSTTRANS)
        self.assertTrue(self.is_on(MESH, "enabled") and self.is_on(MESH, "active"))


# --- The host script and the installers, against stand-ins for Docker and the system ---------
#
# Each test gets a temporary root: the script is copied with its fixed paths (the state, the
# library, the sysctl drop-in, /swapfile, ...) pointed there, and stand-ins for the commands it
# calls stand first in PATH. A script that stopped using a path the tests point away would fail
# the "contains" check below, not run against the real one.

LINUX = sys.platform.startswith("linux")
# On Windows the bash must be Git's (MSYS), not the launcher of WSL that may stand earlier in PATH.
BASH = next((path for path in [shutil.which("bash")] if path and (sys.platform != "win32" or "git" in path.lower())), None)
DD = shutil.which("dd")
SERVER_SCRIPT = "packages/linux/server/ghostlink-server.sh"
INSTALL_SERVER = "scripts/install-server.sh"
INSTALL_MESH = "scripts/install-mesh.sh"


def put(path, text=""):
    """Write a file with exactly the line ends the text has (write_text would make them CRLF on Windows)."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(text.encode())


def dd_takes_nofollow():
    """GNU dd on Linux opens with O_NOFOLLOW; the dd of Git Bash has no such flag."""
    if not DD:
        return False
    return subprocess.run([DD, "if=/dev/null", "of=/dev/null", "iflag=nofollow"], capture_output=True).returncode == 0


DD_NOFOLLOW = dd_takes_nofollow()

STAND_IN_ID = r"""#!/bin/sh
case "$*" in
    -u) echo 0 ;;
    "-u ghostlink-server" | "-g ghostlink-server") echo 1001 ;;
    *) exit 1 ;;
esac
"""

# FAKE_* variables say what the daemon is like. A file at FAKE_DOCKER_DOWN means it is stopped, at
# FAKE_IPSEC_CONTAINER that the IPsec container exists. FAKE_IPSEC_DIR is the exchange directory
# (where the component writes status.json and the server desired.json), FAKE_SERVER_MOUNTS the
# directories the server container has mounted (by default: the exchange directory, as made by a
# setup of this version). The hooks run when a container is created: FAKE_RUN_HOOK for the IPsec
# component, FAKE_SERVER_HOOK for the server.
STAND_IN_DOCKER = r"""#!/bin/sh
echo "docker $*" >> "$FAKE_LOG"
last=
for argument in "$@"; do last=$argument; done
case "$1" in
    info) [ ! -e "$FAKE_DOCKER_DOWN" ] ;;
    load) exit "${FAKE_LOAD_STATUS:-0}" ;;
    inspect)
        case "$last" in
            ghostlink-server)
                case "$*" in
                    *Mounts*) printf '%s\n' ${FAKE_SERVER_MOUNTS-$FAKE_IPSEC_DIR} ;;
                    *RestartCount*) echo "${FAKE_SERVER_RUNNING:-true} 0" ;;
                    *) echo "${FAKE_SERVER_RUNNING:-true}" ;;
                esac ;;
            ghostlink-ipsec) [ -e "$FAKE_IPSEC_CONTAINER" ] || exit 1; echo "${FAKE_IPSEC_RUNNING:-true}" ;;
            *) exit 1 ;;
        esac ;;
    exec) [ "${FAKE_ADMIN_STATUS:-0}" = 0 ] || exit "$FAKE_ADMIN_STATUS"; echo "ADMIN-OUTPUT $last" ;;
    run)
        [ "${FAKE_RUN_STATUS:-0}" = 0 ] || exit "$FAKE_RUN_STATUS"
        case "$*" in
            *"--name ghostlink-ipsec "*)
                : > "$FAKE_IPSEC_CONTAINER"
                [ -z "${FAKE_RUN_HOOK:-}" ] || sh -c "$FAKE_RUN_HOOK"
                if [ -n "${FAKE_STATUS_JSON:-}" ]; then printf '%s\n' "$FAKE_STATUS_JSON" > "$FAKE_IPSEC_DIR/status.json"; fi ;;
            *"--name ghostlink-server "*)
                [ -z "${FAKE_SERVER_HOOK:-}" ] || sh -c "$FAKE_SERVER_HOOK" ;;
        esac ;;
    rm) case "$*" in *ghostlink-ipsec*) rm -f "$FAKE_IPSEC_CONTAINER" ;; esac ;;
    *) exit 0 ;;
esac
"""

# sleep that does not wait. With FAKE_SETTINGS_ON_SLEEP, the server "writes" the component's
# settings at the first sleep after the server container was made and its image list was read
# (the setup does that right after the container is up): a server that is a little slow.
STAND_IN_SLEEP = r"""#!/bin/sh
echo "sleep $*" >> "$FAKE_LOG"
if [ -n "${FAKE_SETTINGS_ON_SLEEP:-}" ] && grep -q '^docker image ls .* ghostlink-server$' "$FAKE_LOG"; then
    printf '{}\n' > "$FAKE_IPSEC_DIR/desired.json"
fi
exit 0
"""

STAND_IN_SYSTEMCTL_DOCKER = r"""#!/bin/sh
echo "systemctl $*" >> "$FAKE_LOG"
if [ "$*" = "start --no-block docker.service" ]; then
    [ "${FAKE_SYSTEMCTL_STATUS:-0}" != 0 ] || rm -f "$FAKE_DOCKER_DOWN"
    exit "${FAKE_SYSTEMCTL_STATUS:-0}"
fi
exit 0
"""

STAND_IN_SYSCTL = r"""#!/bin/sh
echo "sysctl $*" >> "$FAKE_LOG"
for argument in "$@"; do
    case "$argument" in net.ipv4.ip_forward=*) echo "${argument#*=}" > "$FAKE_FORWARD" ;; esac
done
"""

STAND_IN_INSTALL = r"""#!/bin/sh
for argument in "$@"; do last=$argument; done
mkdir -p "$last"
"""

STAND_IN_QUIET = "#!/bin/sh\necho \"$(basename \"$0\") $*\" >> \"$FAKE_LOG\"\nexit 0\n"


def dd_without_nofollow(real):
    return f"""#!/bin/sh
# dd as GNU has it, for a dd that lacks iflag=nofollow (Git Bash): the flag is left out.
n=$#
while [ "$n" -gt 0 ]; do
    argument=$1; shift; n=$((n - 1))
    case "$argument" in iflag=*) ;; *) set -- "$@" "$argument" ;; esac
done
exec "{real}" "$@"
"""


class StandIns(unittest.TestCase):
    """A temporary root, with stand-in commands first in PATH and a log of their calls."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / "bin").mkdir()
        self.log = self.root / "calls.log"
        self.log.write_bytes(b"")
        if DD and not DD_NOFOLLOW:
            self.stand_in("dd", dd_without_nofollow(Path(DD).as_posix()))

    def stand_in(self, name, text):
        path = self.root / "bin" / name
        path.write_bytes(text.encode())
        path.chmod(0o755)

    def environment(self, **extra):
        environment = dict(os.environ)
        environment["PATH"] = str(self.root / "bin") + os.pathsep + environment.get("PATH", "")
        environment["FAKE_LOG"] = self.log.as_posix()
        environment.update(extra)
        return environment

    def calls(self):
        return self.log.read_text().splitlines()

    def patched(self, script, replacements):
        text = (ROOT / script).read_text(encoding="utf-8")
        for old, new in replacements:
            self.assertIn(old, text, f"{script} no longer contains {old!r}, which these tests point at a temporary root")
            text = text.replace(old, new)
        return text


@unittest.skipUnless(SHELL, "a POSIX shell is needed to run the host script")
class ServerHostScript(StandIns):
    """packages/linux/server/ghostlink-server.sh, run by the shell Debian uses for scripts."""

    def setUp(self):
        super().setUp()
        for directory in ("lib", "state", "run-systemd"):
            (self.root / directory).mkdir()
        put(self.root / "lib/image.env", "IMAGE=ghostlink-server:0.0.0\nIPSEC_IMAGE=ghostlink-ipsec:0.0.0\n")
        self.state = self.root / "state"
        self.sysctl = self.root / "90-ghostlink-ipsec.conf"
        self.forward = self.root / "ip_forward"
        put(self.forward, "1\n")
        self.container = self.root / "ipsec-container"
        self.docker_down = self.root / "docker-down"
        # The exchange directory is not made here: the script makes it, as it does in /var/lib.
        self.ipsec = self.root / "ghostlink-ipsec"
        for name, text in (("id", STAND_IN_ID), ("docker", STAND_IN_DOCKER), ("systemctl", STAND_IN_SYSTEMCTL_DOCKER),
                           ("sysctl", STAND_IN_SYSCTL), ("install", STAND_IN_INSTALL), ("chown", STAND_IN_QUIET),
                           ("modprobe", STAND_IN_QUIET), ("ip", STAND_IN_QUIET), ("sleep", STAND_IN_SLEEP)):
            self.stand_in(name, text)
        # chmod really changes the mode (on Linux the tests check it), and says what it was asked.
        self.stand_in("chmod", f'#!/bin/sh\necho "chmod $*" >> "$FAKE_LOG"\nexec "{Path(shutil.which("chmod")).as_posix()}" "$@"\n')
        text = self.patched(SERVER_SCRIPT, [
            ("LIB=/usr/lib/ghostlink-server\n", f"LIB={self.root.as_posix()}/lib\n"),
            ("STATE=/var/lib/ghostlink-server\n", f"STATE={self.state.as_posix()}\n"),
            ("IPSEC_DIR=/var/lib/ghostlink-ipsec\n", f"IPSEC_DIR={self.ipsec.as_posix()}\n"),
            ("SYSCTL_FILE=/etc/sysctl.d/90-ghostlink-ipsec.conf\n", f"SYSCTL_FILE={self.sysctl.as_posix()}\n"),
            ("/proc/sys/net/ipv4/ip_forward", self.forward.as_posix()),
            ("/run/systemd/system", f"{self.root.as_posix()}/run-systemd"),
        ])
        self.script = self.root / "ghostlink-server"
        self.script.write_bytes(text.encode())

    def run_host(self, *arguments, **extra):
        environment = self.environment(FAKE_STATE=self.state.as_posix(), FAKE_FORWARD=self.forward.as_posix(),
                                       FAKE_IPSEC_CONTAINER=self.container.as_posix(), FAKE_IPSEC_DIR=self.ipsec.as_posix(),
                                       FAKE_DOCKER_DOWN=self.docker_down.as_posix(), **extra)
        return subprocess.run([SHELL, self.script.as_posix(), *arguments], env=environment,
                              capture_output=True, text=True, timeout=120)

    def secret_file(self):
        secret = self.root / "somebody-elses-file"
        put(secret, '  "error": "TOP-SECRET"\n{"apiUrl": "TOP-SECRET"}\nTOP-SECRET\n')
        return secret

    # What root prints from the state.

    KEY = '{"apiUrl":"https://203.0.113.7:40001/abc","certSha256":"AB"}'
    STATE_JSON = '{\n  "hostname": "203.0.113.7",\n  "api": {\n    "port": 40001\n  },\n  "shadowsocks": {\n    "enabled": true,\n    "port": 8388\n  }\n}\n'

    def test_show_key_prints_the_key(self):
        put(self.state / "management-key.json", self.KEY)
        result = self.run_host("show-key")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(self.KEY, result.stdout)

    def test_show_key_without_a_server_says_so(self):
        result = self.run_host("show-key")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no server yet", result.stderr)

    @unittest.skipUnless(LINUX and DD_NOFOLLOW, "needs symlinks and a dd that opens with O_NOFOLLOW")
    def test_show_key_never_prints_the_file_a_symlink_points_at(self):
        (self.state / "management-key.json").symlink_to(self.secret_file())
        result = self.run_host("show-key")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("TOP-SECRET", result.stdout + result.stderr)

    @unittest.skipUnless(LINUX and DD_NOFOLLOW, "needs FIFOs and a dd that opens with O_NOFOLLOW")
    def test_a_pipe_in_the_place_of_a_state_file_is_not_waited_on(self):
        os.mkfifo(self.state / "management-key.json")
        result = self.run_host("show-key")  # a read that waited for a writer would end in the timeout
        self.assertNotEqual(result.returncode, 0)

    def test_status_summarises_the_state(self):
        put(self.state / "state.json", self.STATE_JSON)
        result = self.run_host("status")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("address in keys: 203.0.113.7", result.stdout)
        self.assertIn("management API: TCP 40001", result.stdout)
        self.assertIn("Shadowsocks: TCP and UDP 8388, on", result.stdout)

    @unittest.skipUnless(LINUX and DD_NOFOLLOW, "needs symlinks and a dd that opens with O_NOFOLLOW")
    def test_status_summarises_nothing_from_a_symlinked_state_file(self):
        secret = self.root / "other-state.json"
        put(secret, self.STATE_JSON.replace("203.0.113.7", "TOP-SECRET"))
        (self.state / "state.json").symlink_to(secret)
        result = self.run_host("status")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("TOP-SECRET", result.stdout + result.stderr)

    STATUS = '{\n  "running": true,\n  "applied_generation": 3,\n  "sessions": [\n    {\n      "identity": "alice@ghostlink"\n    }\n  ]\n}\n'

    def test_ipsec_status_reads_what_the_component_reports(self):
        put(self.ipsec / "status.json", self.STATUS)
        put(self.container, "")
        result = self.run_host("ipsec", "status")
        self.assertEqual(result.returncode, 0, result.stderr)
        for line in ("  running: true", "  applied_generation: 3", "  connections: 1"):
            self.assertIn(line, result.stdout)

    def test_ipsec_status_ignores_a_report_in_the_state(self):
        # 0.9.1 and earlier exchanged the files in the state's ipsec directory, which the server
        # account can fill with whatever it likes: nothing reads it any more.
        put(self.state / "ipsec/status.json", self.STATUS)
        put(self.container, "")
        result = self.run_host("ipsec", "status")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("running: true", result.stdout)
        self.assertIn("connections: 0", result.stdout)

    @unittest.skipUnless(LINUX and DD_NOFOLLOW, "needs symlinks and a dd that opens with O_NOFOLLOW")
    def test_ipsec_status_never_reads_through_a_symlink(self):
        put(self.container, "")
        other = self.root / "other-directory"
        other.mkdir()
        put(other / "status.json", self.STATUS.replace('"running": true', '"error": "TOP-SECRET"'))
        for arrangement in ("directory", "file"):
            with self.subTest(arrangement=arrangement):
                shutil.rmtree(self.ipsec, ignore_errors=True)
                if self.ipsec.is_symlink():
                    self.ipsec.unlink()
                if arrangement == "directory":
                    self.ipsec.symlink_to(other, target_is_directory=True)
                else:
                    self.ipsec.mkdir()
                    (self.ipsec / "status.json").symlink_to(other / "status.json")
                result = self.run_host("ipsec", "status")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn("TOP-SECRET", result.stdout + result.stderr)
                self.assertIn("connections: 0", result.stdout)

    # A private file for the output of an admin command.

    def test_kit_saves_its_output_in_a_new_private_file(self):
        output = self.root / "kit.html"
        result = self.run_host("kit", "alice", "--output", output.as_posix())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output.read_text().strip(), "ADMIN-OUTPUT alice")
        if LINUX:
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)

    def test_a_kit_that_fails_leaves_no_file(self):
        output = self.root / "kit.html"
        for arrangement, extra in (("the command fails", {"FAKE_ADMIN_STATUS": "3"}),
                                   ("the server is not running (a failed check, which exits)", {"FAKE_SERVER_RUNNING": "false"})):
            with self.subTest(arrangement=arrangement):
                result = self.run_host("kit", "alice", "--output", output.as_posix(), **extra)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists(), "a failed kit left its file")

    def test_a_kit_never_replaces_a_file_that_is_there(self):
        output = self.root / "kit.html"
        put(output, "somebody's file")
        result = self.run_host("kit", "alice", "--output", output.as_posix())
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already exists", result.stderr)
        self.assertEqual(output.read_text(), "somebody's file")

    def test_a_kit_that_cannot_create_its_file_removes_nothing(self):
        # What was not created here is not this script's to remove: it never calls rm for it.
        real_rm = shutil.which("rm")
        self.stand_in("rm", f'#!/bin/sh\necho "rm $*" >> "$FAKE_LOG"\nexec "{Path(real_rm).as_posix()}" "$@"\n')
        output = self.root / "no-such-directory" / "kit.html"
        result = self.run_host("kit", "alice", "--output", output.as_posix())
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual([call for call in self.calls() if call.startswith("rm ")], [])

    @unittest.skipUnless(LINUX, "needs symlinks")
    def test_a_kit_never_writes_through_a_symlink(self):
        target = self.root / "target.txt"
        put(target, "precious")
        output = self.root / "kit.html"
        output.symlink_to(target)
        result = self.run_host("kit", "alice", "--output", output.as_posix())
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(target.read_text(), "precious")
        self.assertTrue(output.is_symlink())

    # "ipsec enable" changes the host only for a component that runs.

    RUNNING = '{"running": true}'

    def test_ipsec_enable_turns_forwarding_on_and_keeps_it(self):
        put(self.forward, "0\n")
        result = self.run_host("ipsec", "enable", FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("The IPsec component is running", result.stdout)
        self.assertEqual(self.forward.read_text().strip(), "1")
        self.assertIn("net.ipv4.ip_forward = 1", self.sysctl.read_text())

    def test_an_ipsec_enable_that_fails_before_the_component_exists_changes_nothing(self):
        for arrangement, extra in (("the image does not load", {"FAKE_LOAD_STATUS": "1"}),
                                   ("docker run fails", {"FAKE_RUN_STATUS": "1"})):
            with self.subTest(arrangement=arrangement):
                put(self.forward, "0\n")
                self.sysctl.unlink(missing_ok=True)
                result = self.run_host("ipsec", "enable", **extra)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.sysctl.exists(), "the sysctl drop-in was left behind")
                self.assertEqual(self.forward.read_text().strip(), "0", "IPv4 forwarding was left on")

    def test_a_failed_ipsec_enable_keeps_what_an_earlier_one_set_up(self):
        put(self.sysctl, "net.ipv4.ip_forward = 1\n")
        put(self.forward, "1\n")
        result = self.run_host("ipsec", "enable", FAKE_LOAD_STATUS="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.sysctl.exists())
        self.assertEqual(self.forward.read_text().strip(), "1")

    def test_a_component_that_is_slow_to_start_keeps_forwarding(self):
        # Once the container exists it may still come up, and it needs forwarding to do so.
        put(self.forward, "0\n")
        result = self.run_host("ipsec", "enable", FAKE_IPSEC_RUNNING="false")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("did not start", result.stderr)
        self.assertTrue(self.sysctl.exists())
        self.assertEqual(self.forward.read_text().strip(), "1")

    # The exchange directory: root's, outside the state, so the server account cannot replace it.

    def mounts(self):
        """The --volume arguments of every docker run, as {container name: [mounts]}."""
        found = {}
        for call in self.calls():
            if call.startswith("docker run ") and " --name " in call:
                name = call.split(" --name ")[1].split()[0]
                found[name] = re.findall(r"--volume (\S+)", call)
        return found

    def test_ipsec_enable_makes_the_directory_root_s_with_the_accounts_group(self):
        result = self.run_host("ipsec", "enable", FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.ipsec.is_dir())
        # Owner root, group of the server account (the stand-in id says 1001), set on "." inside the directory.
        self.assertIn("chown 0:1001 .", self.calls())
        self.assertIn("chmod 2770 .", self.calls())
        if LINUX:
            self.assertEqual(self.ipsec.stat().st_mode & 0o7777, 0o2770)

    def test_a_report_of_an_earlier_run_is_not_taken_for_the_component_that_starts(self):
        # The component is up once the new status.json says so: a "running" left by an earlier run
        # (the component that was removed, or a status the server account put there) must not do.
        put(self.ipsec / "status.json", self.RUNNING)
        result = self.run_host("ipsec", "enable", FAKE_IPSEC_RUNNING="false")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("did not start", result.stderr)

    def test_the_component_mounts_the_exchange_directory_and_not_the_state(self):
        result = self.run_host("ipsec", "enable", FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.mounts()["ghostlink-ipsec"], [f"{self.ipsec.as_posix()}:{self.ipsec.as_posix()}"])
        run = [call for call in self.calls() if call.startswith("docker run ") and "--name ghostlink-ipsec " in call][0]
        self.assertIn(f"serve --dir {self.ipsec.as_posix()} ", run)
        self.assertNotIn(self.state.as_posix(), run)

    def test_the_directory_is_given_its_owner_and_mode_again_at_each_start(self):
        self.ipsec.mkdir(mode=0o777)
        self.ipsec.chmod(0o777)
        result = self.run_host("ipsec", "enable", FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("chown 0:1001 .", self.calls())
        if LINUX:
            self.assertEqual(self.ipsec.stat().st_mode & 0o7777, 0o2770)

    def test_ipsec_enable_refuses_a_file_for_the_directory_and_changes_nothing(self):
        put(self.ipsec, "not a directory")
        put(self.forward, "0\n")
        result = self.run_host("ipsec", "enable", FAKE_STATUS_JSON=self.RUNNING)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not a plain directory", result.stderr)
        self.assertFalse(self.sysctl.exists())
        self.assertEqual(self.forward.read_text().strip(), "0")
        self.assertEqual(self.ipsec.read_text(), "not a directory")
        self.assertEqual([call for call in self.calls() if call.startswith("docker run")], [])

    @unittest.skipUnless(LINUX, "needs symlinks")
    def test_ipsec_enable_refuses_a_symlink_for_the_directory_and_changes_nothing(self):
        elsewhere = self.root / "elsewhere"
        elsewhere.mkdir()
        self.ipsec.symlink_to(elsewhere, target_is_directory=True)
        put(self.forward, "0\n")
        result = self.run_host("ipsec", "enable", FAKE_STATUS_JSON=self.RUNNING)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not a plain directory", result.stderr)
        self.assertFalse(self.sysctl.exists())
        self.assertEqual(self.forward.read_text().strip(), "0")
        self.assertEqual(list(elsewhere.iterdir()), [])
        self.assertEqual([call for call in self.calls() if call.startswith("docker run")], [])

    @unittest.skipUnless(LINUX, "needs symlinks")
    def test_a_symlink_at_the_old_place_changes_nothing(self):
        # 0.9.1 and earlier mounted $STATE/ipsec, so a symlink the server account put there could
        # send root's Docker mount anywhere. Nothing mounts, reads or enters it now.
        elsewhere = self.root / "elsewhere"
        elsewhere.mkdir()
        (self.state / "ipsec").symlink_to(elsewhere, target_is_directory=True)
        # And one put there while the component is created, as the old script's race had it.
        hook = 'rm -rf "$FAKE_STATE/ipsec" && ln -s "$FAKE_OTHER" "$FAKE_STATE/ipsec"'
        result = self.run_host("ipsec", "enable", FAKE_RUN_HOOK=hook, FAKE_OTHER=elsewhere.as_posix(),
                               FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(list(elsewhere.iterdir()), [])
        for container, mounts in self.mounts().items():
            self.assertNotIn(f"{self.state.as_posix()}/ipsec", " ".join(mounts), container)
        self.assertEqual(self.mounts()["ghostlink-ipsec"], [f"{self.ipsec.as_posix()}:{self.ipsec.as_posix()}"])

    def test_ipsec_enable_stops_for_a_server_container_that_does_not_share_the_directory(self):
        # Made by 0.9.1, or by a setup that did not finish: it would write the settings to the state,
        # where a component that reads the exchange directory never looks.
        put(self.forward, "0\n")
        result = self.run_host("ipsec", "enable", FAKE_STATUS_JSON=self.RUNNING, FAKE_SERVER_MOUNTS=self.state.as_posix())
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("sudo ghostlink-server setup", result.stderr)
        self.assertFalse(self.sysctl.exists())
        self.assertEqual(self.forward.read_text().strip(), "0")
        self.assertEqual([call for call in self.calls() if call.startswith("docker run")], [])

    def test_ipsec_disable_clears_the_report_and_leaves_the_directory(self):
        put(self.container, "")
        put(self.ipsec / "status.json", self.STATUS)
        result = self.run_host("ipsec", "disable")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.ipsec / "status.json").exists())
        self.assertTrue(self.ipsec.is_dir())

    # setup: the server gets the directory, and an upgrade moves the component to it.

    def set_up_server(self, **files):
        put(self.state / "management-key.json", self.KEY)
        for name, text in files.items():
            put(self.state / name.replace("__", "/"), text)

    def test_setup_makes_the_directory_before_the_server_that_mounts_it(self):
        self.set_up_server()
        result = self.run_host("setup")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.ipsec.is_dir())
        self.assertIn("chown 0:1001 .", self.calls())
        self.assertEqual(self.mounts()["ghostlink-server"],
                         [f"{self.state.as_posix()}:{self.state.as_posix()}", f"{self.ipsec.as_posix()}:{self.ipsec.as_posix()}"])
        run = [call for call in self.calls() if call.startswith("docker run ") and "--name ghostlink-server " in call][0]
        self.assertTrue(run.endswith(f"serve --state {self.state.as_posix()} --ipsec-dir {self.ipsec.as_posix()}"), run)
        # No IPsec component is made for a host that never enabled it.
        self.assertNotIn("ghostlink-ipsec", self.mounts())

    def test_setup_refuses_something_else_in_place_of_the_directory_before_it_stops_anything(self):
        self.set_up_server()
        put(self.ipsec, "not a directory")
        result = self.run_host("setup")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not a plain directory", result.stderr)
        self.assertEqual([call for call in self.calls() if call.startswith(("docker stop", "docker run", "docker rm"))], [])

    UPGRADE_STATE = '{\n  "ipsec": {\n    "ikev2_enabled": true,\n    "ipsec_enabled": false\n  }\n}\n'

    def old_layout(self):
        """What 0.9.1 left: the component's files in the state, and its container."""
        self.set_up_server(**{"state.json": self.UPGRADE_STATE, "ipsec__desired.json": '{"old": "OLD-SECRET"}',
                              "ipsec__status.json": self.STATUS})
        put(self.container, "")

    def settings_present_when_the_component_starts(self):
        witness = self.root / "witness"
        hook = f'[ -s "$FAKE_IPSEC_DIR/desired.json" ] && : > "{witness.as_posix()}"; true'
        return witness, hook

    def test_an_upgrade_recreates_the_component_on_the_exchange_directory_after_the_server_wrote_its_settings(self):
        self.old_layout()
        witness, hook = self.settings_present_when_the_component_starts()
        result = self.run_host("setup", FAKE_SETTINGS_ON_SLEEP="1", FAKE_RUN_HOOK=hook, FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        runs = [call for call in calls if call.startswith("docker run ") and " --name " in call]
        self.assertEqual([call.split(" --name ")[1].split()[0] for call in runs], ["ghostlink-server", "ghostlink-ipsec"])
        self.assertEqual(self.mounts()["ghostlink-ipsec"], [f"{self.ipsec.as_posix()}:{self.ipsec.as_posix()}"])
        self.assertIn(f"serve --dir {self.ipsec.as_posix()} ", runs[1])
        self.assertTrue(witness.exists(), "the component was created before the server had written its settings")
        self.assertNotIn("has not written", result.stderr)

    def test_an_upgrade_goes_on_when_the_server_is_slow_with_its_settings(self):
        self.old_layout()
        result = self.run_host("setup", FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("has not written the IPsec settings", result.stderr)
        self.assertIn("ghostlink-ipsec", self.mounts())

    def test_an_upgrade_does_not_wait_for_settings_that_nothing_writes(self):
        # IKEv2 and IPsec are off: the server writes nothing at its start, and there is nothing to wait for.
        self.old_layout()
        put(self.state / "state.json", self.STATE_JSON)
        result = self.run_host("setup", FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("has not written", result.stderr)
        calls = self.calls()
        server_up = [number for number, call in enumerate(calls) if call.startswith("docker image ls") and call.endswith("ghostlink-server")][0]
        self.assertEqual([call for call in calls[server_up:] if call.startswith("sleep")], [], "setup waited for settings that are not coming")
        self.assertIn("ghostlink-ipsec", self.mounts())

    def test_an_upgrade_takes_the_old_directory_away_and_never_reads_it(self):
        self.old_layout()
        result = self.run_host("setup", FAKE_SETTINGS_ON_SLEEP="1", FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.state / "ipsec").exists(), "the old exchange directory was left in the state")
        # The settings in the new place are the server's, not a copy of the old file.
        self.assertNotIn("OLD-SECRET", (self.ipsec / "desired.json").read_text())

    def test_what_else_is_in_the_old_directory_stays(self):
        self.old_layout()
        put(self.state / "ipsec/notes.txt", "mine")
        result = self.run_host("setup", FAKE_SETTINGS_ON_SLEEP="1", FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([path.name for path in (self.state / "ipsec").iterdir()], ["notes.txt"])

    @unittest.skipUnless(LINUX, "needs symlinks")
    def test_a_symlink_in_the_old_place_is_unlinked_and_not_followed(self):
        self.old_layout()
        shutil.rmtree(self.state / "ipsec")
        elsewhere = self.root / "elsewhere"
        put(elsewhere / "desired.json", "PRECIOUS")
        (self.state / "ipsec").symlink_to(elsewhere, target_is_directory=True)
        result = self.run_host("setup", FAKE_SETTINGS_ON_SLEEP="1", FAKE_STATUS_JSON=self.RUNNING)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.state / "ipsec").is_symlink() or (self.state / "ipsec").exists())
        self.assertEqual((elsewhere / "desired.json").read_text(), "PRECIOUS")

    def test_a_setup_that_cannot_start_the_component_keeps_the_old_directory(self):
        # Until the component runs on the new directory, nothing is taken away.
        self.old_layout()
        result = self.run_host("setup", FAKE_SETTINGS_ON_SLEEP="1", FAKE_IPSEC_RUNNING="false")
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.state / "ipsec/desired.json").exists())

    # The package scripts: a purge takes the exchange directory away with the state.

    def package_script(self, name, *arguments):
        text = self.patched(f"packages/linux/scripts/{name}", [
            ("/var/lib/ghostlink-server", self.state.as_posix()), ("/var/lib/ghostlink-ipsec", self.ipsec.as_posix())])
        script = self.root / name
        script.write_bytes(text.encode())
        self.stand_in("userdel", STAND_IN_QUIET)
        return subprocess.run([SHELL, script.as_posix(), *arguments], env=self.environment(), capture_output=True, text=True, timeout=60)

    def test_a_purge_deletes_the_exchange_directory_with_the_state_and_a_removal_keeps_both(self):
        put(self.ipsec / "desired.json", "{}")
        self.set_up_server()
        for action in ("remove", "upgrade"):
            result = self.package_script("server-postremove.sh", action)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue((self.ipsec / "desired.json").exists() and (self.state / "management-key.json").exists(), action)
        result = self.package_script("server-postremove.sh", "purge")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.ipsec.exists(), "a purge left the exchange directory")
        self.assertFalse(self.state.exists(), "a purge left the state")
        self.assertIn("userdel ghostlink-server", self.calls())

    # remove: a Docker that is not running cannot remove the containers, which then start with it.

    def test_remove_starts_a_stopped_docker_and_removes_the_containers(self):
        put(self.docker_down, "")
        (self.root / "run-systemd").mkdir(exist_ok=True)
        result = self.run_host("remove")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("systemctl start --no-block docker.service", self.calls())
        self.assertIn("docker rm --force ghostlink-server", self.calls())

    def test_remove_says_so_when_docker_cannot_be_started(self):
        put(self.docker_down, "")
        result = self.run_host("remove", FAKE_SYSTEMCTL_STATUS="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("were not removed", result.stderr)
        self.assertIn("docker rm --force ghostlink-server ghostlink-ipsec", result.stderr)
        self.assertNotIn("docker rm --force ghostlink-server", self.calls())

    def test_the_state_is_read_only_through_the_checked_helpers(self):
        # A new read of a state file that skips them (a plain cat, grep, sed or jq on the path) is what
        # these tests exist to prevent: they would follow a symlink as root.
        lines = (ROOT / SERVER_SCRIPT).read_text(encoding="utf-8").splitlines()
        for number, line in enumerate(lines, 1):
            code = line.split("#", 1)[0]
            for name in ("state.json", "management-key.json", "status.json", "desired.json"):
                # A name in a message is no read, and rm unlinks a symlink without following it.
                allowed = ("state_cat", "ipsec_cat", "rm -f --", 'die "', 'echo "')
                if name in code and not any(helper in code for helper in allowed):
                    self.fail(f"{SERVER_SCRIPT}:{number} touches {name} without state_cat, ipsec_cat or in_state_dir: {line.strip()}")

    def test_only_the_state_and_the_exchange_directory_are_mounted(self):
        # The server account owns the state, so a mount of anything inside it (the old $STATE/ipsec) is
        # one it could redirect. The exchange directory is root's, in /var/lib.
        mounts = []
        for line in (ROOT / SERVER_SCRIPT).read_text(encoding="utf-8").splitlines():
            mounts += re.findall(r'--volume (\S+)', line.split("#", 1)[0])
        self.assertEqual(sorted(mounts), sorted(['"$STATE:$STATE"', '"$IPSEC_DIR:$IPSEC_DIR"', '"$IPSEC_DIR:$IPSEC_DIR"']))


JQ = shutil.which("jq")

# Stand-ins for what the installers run as root on a server. FAKE_* variables make one fail.
STAND_IN_SWAP = {
    "fallocate": '#!/bin/sh\necho "fallocate $*" >> "$FAKE_LOG"\n[ "${FAKE_FALLOCATE_STATUS:-0}" = 0 ] || exit "$FAKE_FALLOCATE_STATUS"\n'
                 'for argument in "$@"; do last=$argument; done\n: > "$last"\n',
    "dd": '#!/bin/sh\necho "dd $*" >> "$FAKE_LOG"\n[ "${FAKE_DD_STATUS:-0}" = 0 ] || exit "$FAKE_DD_STATUS"\n'
          'for argument in "$@"; do case "$argument" in of=*) : > "${argument#of=}" ;; esac; done\n',
    "mkswap": '#!/bin/sh\necho "mkswap $*" >> "$FAKE_LOG"\nexit "${FAKE_MKSWAP_STATUS:-0}"\n',
    "swapon": '#!/bin/sh\necho "swapon $*" >> "$FAKE_LOG"\nexit "${FAKE_SWAPON_STATUS:-0}"\n',
    "swapoff": '#!/bin/sh\necho "swapoff $*" >> "$FAKE_LOG"\n',
    "chattr": '#!/bin/sh\necho "chattr $*" >> "$FAKE_LOG"\n',
    "stat": '#!/bin/sh\necho "${FAKE_FILESYSTEM:-ext2/ext3}"\n',
}


@unittest.skipUnless(BASH, "bash is needed to run the installers")
class InstallerFunctions(StandIns):
    """Functions of the installers, called by sourcing the script (it runs its main only when run)."""

    def call(self, script, replacements, body, **extra):
        path = self.root / Path(script).name
        path.write_bytes(self.patched(script, replacements).encode())
        return subprocess.run([BASH, "-c", f'. "{path.as_posix()}"\n{body}'], env=self.environment(**extra),
                              capture_output=True, text=True, timeout=120)

    # install-server.sh: the swap file of --add-swap.

    def swap(self, body="add_swap_file", **extra):
        for name, text in STAND_IN_SWAP.items():
            self.stand_in(name, text)
        self.swapfile = self.root / "swapfile"
        self.fstab = self.root / "fstab"
        return self.call(INSTALL_SERVER, [("/swapfile", self.swapfile.as_posix()), ("/etc/fstab", self.fstab.as_posix())], body, **extra)

    def test_a_swap_file_is_made_turned_on_and_kept_across_reboots(self):
        result = self.swap()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.swapfile.exists(), "a swap file that is on was taken away")
        self.assertEqual(self.fstab.read_text().splitlines(), [f"{self.swapfile.as_posix()} none swap sw 0 0"])
        calls = self.calls()
        self.assertIn(f"mkswap {self.swapfile.as_posix()}", calls)
        self.assertIn(f"swapon {self.swapfile.as_posix()}", calls)
        if LINUX:
            self.assertEqual(self.swapfile.stat().st_mode & 0o777, 0o600)

    def test_the_swap_file_is_not_listed_twice_in_fstab(self):
        put(self.root / "fstab", f"{(self.root / 'swapfile').as_posix()} none swap sw 0 0\n")
        result = self.swap()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.fstab.read_text().splitlines()), 1)

    def test_a_swap_file_that_fails_is_taken_away_again(self):
        # Before, a failure left the file behind, and the next run refused it ("already exists").
        for arrangement, extra in (("mkswap fails", {"FAKE_MKSWAP_STATUS": "1"}), ("swapon fails", {"FAKE_SWAPON_STATUS": "1"}),
                                   ("no room for it", {"FAKE_FALLOCATE_STATUS": "1", "FAKE_DD_STATUS": "1"})):
            with self.subTest(arrangement=arrangement):
                result = self.swap(**extra)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.swapfile.exists(), "a failed swap file was left behind")
                self.assertFalse(self.fstab.exists(), "a failed swap file was written to fstab")
                if "swapon fails" in arrangement:
                    self.assertIn(f"swapoff {self.swapfile.as_posix()}", self.calls())

    def test_a_swap_file_made_with_dd_when_fallocate_cannot(self):
        result = self.swap(FAKE_FALLOCATE_STATUS="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any(call.startswith("dd if=/dev/zero of=") for call in self.calls()))
        self.assertTrue(self.swapfile.exists())

    def test_btrfs_gets_copy_on_write_off_before_the_file_is_filled(self):
        result = self.swap(FAKE_FILESYSTEM="btrfs")
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        chattr = f"chattr +C {self.swapfile.as_posix()}"
        self.assertIn(chattr, calls)
        self.assertLess(calls.index(chattr), calls.index(f"fallocate -l 1G {self.swapfile.as_posix()}"))

    def test_other_filesystems_get_no_chattr(self):
        self.swap()
        self.assertFalse([call for call in self.calls() if call.startswith("chattr")])

    def test_a_swap_file_that_is_there_is_left_alone(self):
        for arrangement in ("a file", "a dangling symlink"):
            with self.subTest(arrangement=arrangement):
                swapfile = self.root / "swapfile"
                if swapfile.is_symlink() or swapfile.exists():
                    swapfile.unlink()
                if arrangement == "a file":
                    put(swapfile, "in use")
                elif LINUX:
                    swapfile.symlink_to(self.root / "nowhere")
                else:
                    continue
                result = self.swap()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("already exists", result.stderr)
                self.assertTrue(swapfile.exists() or swapfile.is_symlink(), "a swap file that was not made here was taken away")
                self.assertEqual([call for call in self.calls() if call.startswith(("swapoff", "mkswap", "swapon"))], [])

    # install-server.sh: what it opens in the firewall.

    def firewall(self, body, *, firewalld=True, node=None, **extra):
        put(self.root / "bin/ufw", '#!/bin/sh\necho "ufw $*" >> "$FAKE_LOG"\n'
                                    '[ "$1" != status ] || echo "Status: ${FAKE_UFW_STATE:-inactive}"\n[ "$1" != allow ] || exit "${FAKE_UFW_STATUS:-0}"\n')
        (self.root / "bin/ufw").chmod(0o755)
        if firewalld:
            put(self.root / "bin/firewall-cmd", '#!/bin/sh\necho "firewall-cmd $*" >> "$FAKE_LOG"\n'
                                                 'case "$*" in *--state*) exit 0 ;; *--add-port* | *--change-interface*) exit "${FAKE_FIREWALLD_STATUS:-0}" ;; esac\n')
            (self.root / "bin/firewall-cmd").chmod(0o755)
        node_file = self.root / "node.json"
        if node is not None:
            put(node_file, node)
        return self.call(INSTALL_SERVER, [("/var/lib/ghostlink-mesh/node.json", node_file.as_posix())], body, **extra)

    PORTS = "open_ports '8388/tcp  Shadowsocks' '500/udp  IKEv2/IPsec'"

    def test_firewalld_gets_each_port_permanent_and_live_and_is_never_reloaded(self):
        result = self.firewall(self.PORTS)
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        for port in ("8388/tcp", "500/udp"):
            self.assertIn(f"firewall-cmd --permanent --add-port={port}", calls)
            self.assertIn(f"firewall-cmd --add-port={port}", calls)
        self.assertEqual([call for call in calls if "reload" in call], [], "a reload flushes the rules the agents installed")

    @unittest.skipUnless(JQ, "jq reads the interface out of node.json")
    def test_firewalld_trusts_the_interface_of_a_mesh_node(self):
        result = self.firewall(self.PORTS, node='{"interface": "glmesh7", "listen_port": 51821}')
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        self.assertIn("firewall-cmd --permanent --zone=trusted --change-interface=glmesh7", calls)
        self.assertIn("firewall-cmd --zone=trusted --change-interface=glmesh7", calls)

    def test_firewalld_trusts_nothing_when_this_server_is_no_mesh_node(self):
        self.firewall(self.PORTS)
        self.assertEqual([call for call in self.calls() if "change-interface" in call], [])

    @unittest.skipUnless(JQ, "jq reads the interface out of node.json")
    def test_an_odd_interface_name_is_not_passed_on(self):
        self.firewall(self.PORTS, node='{"interface": "x;reboot"}')
        calls = self.calls()
        self.assertEqual([call for call in calls if "reboot" in call], [])
        self.assertIn("firewall-cmd --zone=trusted --change-interface=glmesh0", calls)

    def test_a_firewall_that_refuses_is_a_warning_and_the_installer_goes_on(self):
        for firewall, extra in (("firewalld", {"FAKE_FIREWALLD_STATUS": "1"}),
                                ("ufw", {"FAKE_UFW_STATE": "active", "FAKE_UFW_STATUS": "1"})):
            with self.subTest(firewall=firewall):
                result = self.firewall(f"{self.PORTS}; echo STILL-RUNNING", firewalld=firewall == "firewalld", **extra)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("STILL-RUNNING", result.stdout)
                self.assertIn("could not allow 8388/tcp", result.stderr)
                self.assertIn("could not allow 500/udp", result.stderr)

    def test_ufw_allows_each_port(self):
        result = self.firewall(self.PORTS, firewalld=False, FAKE_UFW_STATE="active")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("ufw allow 8388/tcp comment Ghostlink server", self.calls())

    # install-server.sh: the state it reads as root, and an installation dpkg left half done.

    def test_the_installer_reads_the_state_without_following_a_symlink(self):
        state = self.root / "state"
        state.mkdir()
        put(state / "state.json", '{"hostname": "203.0.113.7"}')
        replacements = [("STATE=/var/lib/ghostlink-server\n", f"STATE={state.as_posix()}\n")]
        result = self.call(INSTALL_SERVER, replacements, "state_json")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("203.0.113.7", result.stdout)
        if LINUX and DD_NOFOLLOW:
            (state / "state.json").unlink()
            (state / "state.json").symlink_to(self.root / "other.json")
            put(self.root / "other.json", '{"hostname": "TOP-SECRET"}')
            result = self.call(INSTALL_SERVER, replacements, "state_json")
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn("TOP-SECRET", result.stdout + result.stderr)

    def test_only_a_package_dpkg_left_half_done_is_finished(self):
        put(self.root / "bin/dpkg-query", '#!/bin/sh\n[ -n "${FAKE_DPKG_STATUS:-}" ] || exit 1\nprintf "%s" "$FAKE_DPKG_STATUS"\n')
        (self.root / "bin/dpkg-query").chmod(0o755)
        for status, interrupted in (("install ok installed", False), ("deinstall ok config-files", False), ("purge ok not-installed", False),
                                    ("", False), ("install ok unpacked", True), ("install ok half-configured", True),
                                    ("install ok half-installed", True), ("install ok triggers-pending", True)):
            with self.subTest(status=status):
                result = self.call(INSTALL_SERVER, [], "package_interrupted", FAKE_DPKG_STATUS=status)
                self.assertEqual(result.returncode == 0, interrupted, f"{status!r}: {result.stderr}")

    def test_the_installers_run_their_main_only_when_executed(self):
        for script in (INSTALL_SERVER, INSTALL_MESH):
            with self.subTest(script=script):
                path = self.root / Path(script).name
                put(path, (ROOT / script).read_text(encoding="utf-8"))
                for how, command in (("executed", [BASH, path.as_posix(), "--help"]), ("piped", None)):
                    if how == "piped":
                        result = subprocess.run([BASH, "-s", "--", "--help"], input=path.read_text(), capture_output=True, text=True, timeout=60)
                    else:
                        result = subprocess.run(command, capture_output=True, text=True, timeout=60)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("Options:", result.stdout, how)
                sourced = subprocess.run([BASH, "-c", f'. "{path.as_posix()}"; echo SOURCED-ONLY'], capture_output=True, text=True, timeout=60)
                self.assertEqual(sourced.stdout.strip(), "SOURCED-ONLY")

    # install-mesh.sh

    def test_a_package_name_gives_the_version_the_agent_reports(self):
        for name, version in (("ghostlink-mesh_0.9.1-1_amd64.deb", "0.9.1"), ("ghostlink-mesh_0.9.1~rc.1-1_arm64.deb", "0.9.1-rc.1"),
                              ("ghostlink-mesh_0.9.1-rc.1-1_amd64.deb", "0.9.1-rc.1"), ("ghostlink-mesh-0.9.1-1.x86_64.rpm", "0.9.1"),
                              ("ghostlink-mesh-0.9.1~rc.1-1.aarch64.rpm", "0.9.1-rc.1"), ("ghostlink-mesh-0.9.1~rc_1-1.x86_64.rpm", "0.9.1-rc-1")):
            with self.subTest(name=name):
                result = self.call(INSTALL_MESH, [], f'package_version "{name}"')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), version)

    @unittest.skipUnless(JQ, "jq reads the port out of node.json")
    def test_a_node_keeps_the_port_it_joined_with(self):
        node = self.root / "node.json"
        replacements = [("/var/lib/ghostlink-mesh/node.json", node.as_posix())]
        for content, port in (('{"listen_port": 51821}', "51821"), ('{"listen_port": 1}', "1"), ('{"listen_port": 65535}', "65535"),
                              ('{}', ""), ('{"listen_port": 0}', ""), ('{"listen_port": 70000}', ""), ('{"listen_port": "51821; reboot"}', ""),
                              ("not json", "")):
            with self.subTest(node=content):
                put(node, content)
                result = self.call(INSTALL_MESH, replacements, "node_port")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), port)
        node.unlink()
        self.assertEqual(self.call(INSTALL_MESH, replacements, "node_port").stdout.strip(), "")


def has_gnu_tar():
    """The build scripts pack with GNU tar's --owner and --group; another tar (bsdtar on Windows) cannot."""
    tar = shutil.which("tar")
    return bool(tar) and "GNU tar" in subprocess.run([tar, "--version"], capture_output=True, text=True).stdout


@unittest.skipUnless(BASH and has_gnu_tar(), "bash and GNU tar are needed to run the build scripts")
class BuildScripts(StandIns):
    """The Linux package scripts name their files after the version as released, also for a pre-release."""

    HOST = subprocess.run(["uname", "-m"], capture_output=True, text=True).stdout.strip() if shutil.which("uname") else ""
    ARCH = {"x86_64": ("x64", "amd64", "x86_64"), "aarch64": ("arm64", "arm64", "aarch64"), "arm64": ("arm64", "arm64", "aarch64")}.get(HOST)

    def tree(self, *scripts):
        """A repository in miniature: the scripts and templates under test, and the files they copy."""
        repository = self.root / "repository"
        shutil.rmtree(repository, ignore_errors=True)
        for relative in scripts + ("packages/linux/ghostlinkd.service", "packages/linux/ghostlink.nfpm.yaml",
                                   "packages/linux/mesh/ghostlink-mesh.nfpm.yaml"):
            target = repository / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / relative, target)
        put(repository / "LICENSE", "licence\n")
        put(repository / "NOTICE.md", "notice\n")
        put(repository / "LICENSES/GPL-3.0.txt", "gpl\n")
        put(repository / "LICENSES/dependencies/go-example/NOTICE", "dependency\n")
        (repository / "core/outline-worker").mkdir(parents=True)
        self.stand_in("cargo", '#!/bin/sh\nmkdir -p target/release\nfor name in ghostlink ghostlinkd ghostlink-mcp; do : > "target/release/$name"; done\n')
        self.stand_in("go", '#!/bin/sh\nwhile [ $# -gt 0 ]; do\n    if [ "$1" = -o ]; then out=$2; fi\n    shift\ndone\n'
                            'mkdir -p "$(dirname "$out")"\nprintf \'#!/bin/sh\\necho "Ghostlink mesh agent %s"\\n\' "$FAKE_VERSION" > "$out"\nchmod 755 "$out"\n')
        self.stand_in("nfpm", '#!/bin/sh\necho "nfpm $*" >> "$FAKE_LOG"\nwhile [ $# -gt 0 ]; do\n    if [ "$1" = --target ]; then : > "$2"; fi\n    shift\ndone\n')
        return repository

    def build(self, script, version):
        repository = self.tree(script)
        result = subprocess.run([BASH, (repository / script).as_posix(), version], cwd=repository,
                                env=self.environment(FAKE_VERSION=version), capture_output=True, text=True, timeout=180)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return repository, [call.split("--target ")[1] for call in self.calls()]

    @unittest.skipUnless(ARCH, "this machine's architecture is not one the packages are built for")
    def test_the_client_packages_are_named_after_the_version_as_released(self):
        short, goarch, rpm_arch = self.ARCH
        for version in ("0.9.1", "0.9.1-rc.1"):
            with self.subTest(version=version):
                self.log.write_bytes(b"")
                repository, targets = self.build("scripts/build-linux.sh", version)
                directory = f"artifacts/linux-{short}"
                self.assertEqual(targets, [f"{directory}/ghostlink_{version}-1_{goarch}.deb", f"{directory}/ghostlink-{version}-1.{rpm_arch}.rpm"])
                for target in targets:
                    self.assertTrue((repository / target).exists(), target)

    @unittest.skipUnless(ARCH, "this machine's architecture is not one the packages are built for")
    def test_the_mesh_packages_are_named_after_the_version_as_released(self):
        short, goarch, rpm_arch = self.ARCH
        for version in ("0.9.1", "0.9.1-rc.1"):
            with self.subTest(version=version):
                self.log.write_bytes(b"")
                repository, targets = self.build("scripts/build-linux-mesh.sh", version)
                directory = f"artifacts/linux-{short}"
                self.assertEqual(targets, [f"{directory}/ghostlink-mesh_{version}-1_{goarch}.deb", f"{directory}/ghostlink-mesh-{version}-1.{rpm_arch}.rpm"])
                for target in targets:
                    self.assertTrue((repository / target).exists(), target)


class ComponentImage(unittest.TestCase):
    def test_the_ipsec_image_and_the_build_of_its_source_use_the_same_alpine(self):
        """The source bundle is built from the Alpine release the image is built on: one image
        (pinned by digest, which is what makes a rebuild the same) and one aports branch."""
        dockerfile = (ROOT / "packages/linux/server/ipsec.Dockerfile").read_text(encoding="utf-8")
        source = (ROOT / "scripts/ipsec-source.sh").read_text(encoding="utf-8")
        image = re.search(r"^FROM (alpine:[0-9.]+@sha256:[0-9a-f]{64}) AS build$", dockerfile, re.M)
        pinned = re.search(r'^alpine="(alpine:[0-9.]+@sha256:[0-9a-f]{64})"$', source, re.M)
        branch = re.search(r"^branch=([0-9.]+)-stable$", source, re.M)
        self.assertTrue(image and pinned and branch, "the Alpine image or the aports branch is no longer pinned where this test looks")
        self.assertEqual(image.group(1), pinned.group(1))
        self.assertTrue(image.group(1).startswith(f"alpine:{branch.group(1)}."), f"{image.group(1)} is not of the {branch.group(1)}-stable branch")


if __name__ == "__main__":
    unittest.main()
