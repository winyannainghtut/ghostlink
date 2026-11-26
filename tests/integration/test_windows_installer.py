"""The Windows installer: who owns the service after a first install and after an upgrade, which
version a pre-release gets, and what the compiled MSI says about upgrades, the service's restart
after a crash and the desktop app (the last from its tables, read without installing it).

The owner is checked with a small model of how Windows Installer fills GHOSTLINK_OWNER_SID from
packages/windows/Ghostlink.wxs (the release's install test in CI runs the real thing).
"""
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from xml.etree import ElementTree

ROOT = Path(__file__).resolve().parents[2]
WIX = "{http://schemas.microsoft.com/wix/2006/wi}"


class WindowsInstallerOwner(unittest.TestCase):
    """GHOSTLINK_OWNER_SID across the install sequences.

    The model follows Windows Installer: AppSearch sets the property when the previous
    installation saved an owner in the registry, over a value given on the command line. AppSearch
    and Sequence="first" assignments run only in the first sequence: the UI sequence, or the
    execute sequence when the UI is skipped (msiexec /qn). Only public (upper-case) properties
    reach the execute sequence after the UI sequence.
    """

    USER = "S-1-5-21-1000-1000-1000-1001"
    SAVED = "S-1-5-21-1000-1000-1000-1002"
    GIVEN = "S-1-5-21-1000-1000-1000-1003"
    SYSTEM = "S-1-5-18"  # who installs when a deployment tool runs the MSI

    @classmethod
    def setUpClass(cls):
        cls.product = ElementTree.parse(ROOT / "packages/windows/Ghostlink.wxs").getroot().find(WIX + "Product")
        standard = {"AppSearch": 50, "LaunchConditions": 100}
        cls.actions = [(standard["AppSearch"], None)]
        for element in cls.product.iter(WIX + "SetProperty"):
            anchor = element.get("Before") or element.get("After")
            cls.actions.append((standard[anchor] + (-1 if element.get("Before") else 1), element))
        cls.actions.sort(key=lambda action: action[0])

    @staticmethod
    def holds(condition, properties):
        """The conditions of the model: terms joined by AND, each a property, NOT a property, or a
        property compared (= or <>) with a quoted string."""
        condition = (condition or "").strip()
        if not condition:
            return True
        for term in re.split(r"\s+AND\s+", condition):
            comparison = re.fullmatch(r'(\w+)\s*(=|<>)\s*"([^"]*)"', term)
            if comparison:
                name, operator, value = comparison.groups()
                equal = properties.get(name, "") == value
                holds = equal if operator == "=" else not equal
            elif term.startswith("NOT "):
                holds = not properties.get(term[4:].strip())
            elif re.fullmatch(r"\w+", term):
                holds = bool(properties.get(term))
            else:
                raise AssertionError(f"the model does not handle the condition {condition!r}")
            if not holds:
                return False
        return True

    def owner(self, given=None, saved=None, interactive=True, user=None):
        properties = {"UserSID": user or self.USER}
        if given:
            properties["GHOSTLINK_OWNER_SID"] = given
        sequences = ["ui", "execute"] if interactive else ["execute"]
        for sequence in sequences:
            if sequence != sequences[0]:
                properties = {name: value for name, value in properties.items() if name.isupper() or name == "UserSID"}
            for _, element in self.actions:
                scheduled = "first" if element is None else element.get("Sequence", "both")
                if (scheduled == "first" and sequence != sequences[0]) or scheduled not in ("first", "both", sequence):
                    continue
                if element is None:
                    if saved:
                        properties["GHOSTLINK_OWNER_SID"] = saved
                elif self.holds(element.text, properties):
                    value = re.sub(r"\[(\w+)\]", lambda match: properties.get(match.group(1), ""), element.get("Value"))
                    if value:
                        properties[element.get("Id")] = value
                    else:
                        properties.pop(element.get("Id"), None)
        return properties.get("GHOSTLINK_OWNER_SID")

    def test_a_first_install_belongs_to_the_installing_user_or_the_given_sid(self):
        for interactive in (True, False):
            with self.subTest(interactive=interactive):
                self.assertEqual(self.owner(interactive=interactive), self.USER)
                self.assertEqual(self.owner(given=self.GIVEN, interactive=interactive), self.GIVEN)

    def test_an_upgrade_keeps_the_saved_owner(self):
        for interactive in (True, False):
            with self.subTest(interactive=interactive):
                self.assertEqual(self.owner(saved=self.SAVED, interactive=interactive), self.SAVED)

    def test_an_upgrade_with_a_given_sid_changes_the_owner(self):
        for interactive in (True, False):
            with self.subTest(interactive=interactive):
                self.assertEqual(self.owner(given=self.GIVEN, saved=self.SAVED, interactive=interactive), self.GIVEN)

    def test_the_service_and_the_saved_owner_use_the_property(self):
        service = self.product.find(f".//{WIX}ServiceInstall")
        self.assertIn('--owner-sid "[GHOSTLINK_OWNER_SID]"', service.get("Arguments"))
        saved = [value for value in self.product.iter(WIX + "RegistryValue") if value.get("Name") == "OwnerSID"]
        self.assertEqual([value.get("Value") for value in saved], ["[GHOSTLINK_OWNER_SID]"])

    def test_a_deployment_tool_installing_as_system_must_name_the_owner(self):
        # SYSTEM is nobody to own the service for: no owner is picked for it, so the launch
        # condition (which was dead while the owner was always filled in) refuses the install.
        for interactive in (True, False):
            with self.subTest(interactive=interactive):
                self.assertIsNone(self.owner(user=self.SYSTEM, interactive=interactive))
        conditions = {condition.get("Message"): (condition.text or "").strip() for condition in self.product.findall(WIX + "Condition")}
        self.assertEqual(
            conditions["The authorized user's SID is required. Supply GHOSTLINK_OWNER_SID when installing unattended."],
            "Installed OR GHOSTLINK_OWNER_SID")

    def test_system_installs_for_a_named_owner_and_an_upgrade_keeps_the_saved_one(self):
        for interactive in (True, False):
            with self.subTest(interactive=interactive):
                self.assertEqual(self.owner(user=self.SYSTEM, given=self.GIVEN, interactive=interactive), self.GIVEN)
                self.assertEqual(self.owner(user=self.SYSTEM, saved=self.SAVED, interactive=interactive), self.SAVED)


POWERSHELL = shutil.which("pwsh") or shutil.which("powershell")
# On Windows the first bash on the PATH can be the WSL launcher (System32\bash.exe), which runs
# nothing without a Linux distribution: only Git's bash counts there, as in test_linux_packages.py.
BASH = next((path for path in [shutil.which("bash")] if path and (sys.platform != "win32" or "git" in path.lower())), None)


class WindowsInstallerVersion(unittest.TestCase):
    """The product version: Windows Installer takes a numeric a.b.c, so a pre-release tag such as
    0.9.1-rc.1 must not reach Product/@Version (candle refuses it: CNDL0108)."""

    @classmethod
    def setUpClass(cls):
        cls.wxs = (ROOT / "packages/windows/Ghostlink.wxs").read_text(encoding="utf-8")
        cls.script = (ROOT / "scripts/build-windows.ps1").read_text(encoding="utf-8")

    def test_the_installer_gets_the_numeric_core_and_the_full_version_stays_in_the_arp_comments(self):
        self.assertIn('Version="$(var.MsiVersion)"', self.wxs)
        self.assertNotIn('Version="$(var.Version)"', self.wxs)
        self.assertIn('Id="ARPCOMMENTS" Value="Ghostlink $(var.Version)"', self.wxs)
        self.assertIn('"-dMsiVersion=$msiVersion"', self.script)
        self.assertIn('"-dVersion=$Version"', self.script)

    def derive(self, version):
        """Run the derivation of build-windows.ps1 itself in PowerShell: the numeric version, or None when it throws."""
        match = re.search(r"^if \(\$Version -match .*?^\} else \{ throw \"Version '\$Version' is not a semantic version\" \}$", self.script, re.S | re.M)
        self.assertIsNotNone(match, "build-windows.ps1 no longer derives $msiVersion where the test looks for it")
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "derive.ps1"
            path.write_text(f"$ErrorActionPreference = 'Stop'\n$Version = '{version}'\n{match.group(0)}\nWrite-Output $msiVersion\n", encoding="utf-8")
            result = subprocess.run([POWERSHELL, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(path)],
                                    capture_output=True, text=True)
        return result.stdout.strip() if result.returncode == 0 else None

    @unittest.skipUnless(POWERSHELL, "PowerShell is needed to run the derivation")
    def test_a_release_and_its_pre_releases_derive_the_numeric_core(self):
        for version, expected in (("0.9.1", "0.9.1"), ("0.9.1-rc.1", "0.9.1"), ("1.0.0-rc.12", "1.0.0"),
                                  ("10.20.65535-beta-2", "10.20.65535"), ("255.255.65535", "255.255.65535")):
            with self.subTest(version=version):
                self.assertEqual(self.derive(version), expected)

    @unittest.skipUnless(POWERSHELL, "PowerShell is needed to run the derivation")
    def test_a_version_windows_installer_cannot_hold_stops_the_build(self):
        for version in ("0.9", "v0.9.1", "0.9.1+build", "256.0.0", "0.256.0", "0.0.65536", "one.two.three"):
            with self.subTest(version=version):
                self.assertIsNone(self.derive(version))


class ReleaseBuildsRefuseTheTestHook(unittest.TestCase):
    """The service has a hook for its tests (the variable GHOSTLINK_TEST_DYNAMIC_KEY_URL, compiled in only
    with the Cargo feature test-hooks, which the daemon's dev-dependencies turn on). A release build never
    enables it; as defence in depth the build scripts stop when a program they stage contains its name."""

    NAME = b"GHOSTLINK_TEST_DYNAMIC_KEY_URL"

    @classmethod
    def setUpClass(cls):
        cls.windows = (ROOT / "scripts/build-windows.ps1").read_text(encoding="utf-8")
        cls.linux = (ROOT / "scripts/build-linux.sh").read_text(encoding="utf-8")

    def files(self, directory):
        clean = Path(directory) / "clean.exe"
        clean.write_bytes(b"MZ\x00\x01" + b"GHOSTLINK_TEST_DYNAMIC_KEY_UR" + b"\x00" + os.urandom(64))  # one byte short of the name
        hooked = Path(directory) / "hooked.exe"
        hooked.write_bytes(b"MZ\x90\x00" + os.urandom(256) + self.NAME + b"\x00" + os.urandom(256))
        return clean, hooked

    @unittest.skipUnless(POWERSHELL, "PowerShell is needed to run the check")
    def test_the_windows_build_stops_for_a_program_with_the_hook(self):
        match = re.search(r"^function Assert-NoTestHook.*?^}$", self.windows, re.S | re.M)
        self.assertIsNotNone(match, "build-windows.ps1 has no Assert-NoTestHook")
        with tempfile.TemporaryDirectory() as temporary:
            clean, hooked = self.files(temporary)
            script = Path(temporary) / "check.ps1"
            for files, ends_well in (([clean], True), ([clean, hooked], False)):
                script.write_text("$ErrorActionPreference = 'Stop'\n" + match.group(0) + "\nAssert-NoTestHook @(" + ", ".join(f"'{file}'" for file in files) + ")\n", encoding="utf-8")
                result = subprocess.run([POWERSHELL, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(script)], capture_output=True, text=True)
                with self.subTest(files=[file.name for file in files]):
                    self.assertEqual(result.returncode == 0, ends_well, result.stderr)
                    if not ends_well:
                        self.assertIn("hooked.exe contains GHOSTLINK_TEST_DYNAMIC_KEY_URL", result.stderr)
                        self.assertIn("test-hooks", result.stderr)

    def test_the_windows_build_checks_every_staged_program_before_it_packages_them(self):
        call = self.windows.index("Assert-NoTestHook @(Get-ChildItem -LiteralPath $stage -Recurse -Filter '*.exe'")
        self.assertLess(self.windows.index("ghostlink-desktop.exe') -Destination"), call, "the desktop app is staged after the check")
        self.assertLess(call, self.windows.index("& (Join-Path $wixDirectory 'heat.exe')"), "the installer is built before the check")
        self.assertLess(call, self.windows.index("Compress-Archive"), "the portable archive is made before the check")

    @unittest.skipUnless(BASH, "bash is needed to run the check")
    def test_the_linux_build_stops_for_a_program_with_the_hook(self):
        loop = re.search(r"^for program in .*?^done$", self.linux, re.S | re.M)
        self.assertIsNotNone(loop, "build-linux.sh does not look for the hook in what it stages")
        self.assertLess(self.linux.index('install -m 0755 target/release/ghostlink-tunnel "$stage/lib/ghostlink/"'), loop.start())
        self.assertLess(loop.start(), self.linux.index("tar -C"), "the archive is made before the check")
        with tempfile.TemporaryDirectory() as temporary:
            stage = Path(temporary) / "stage"
            (stage / "bin").mkdir(parents=True)
            (stage / "lib/ghostlink").mkdir(parents=True)
            clean, hooked = self.files(temporary)
            shutil.copyfile(clean, stage / "bin/ghostlink")
            shutil.copyfile(clean, stage / "lib/ghostlink/ghostlink-tunnel")
            # Relative to the working directory, whatever bash this is (Git's, Linux's, or WSL's launcher).
            (Path(temporary) / "check.sh").write_text("set -euo pipefail\nstage=stage\n" + loop.group(0) + "\n", encoding="utf-8", newline="\n")
            run = lambda: subprocess.run([BASH, "check.sh"], cwd=temporary, capture_output=True, text=True)
            before = run()
            self.assertEqual(before.returncode, 0, before.stderr)
            shutil.copyfile(hooked, stage / "bin/ghostlinkd")
            result = run()
            self.assertEqual(result.returncode, 1)
            self.assertIn("ghostlinkd contains GHOSTLINK_TEST_DYNAMIC_KEY_URL", result.stderr)


class PackageTestScripts(unittest.TestCase):
    """The scripts that CI runs to install the packages for real (tests/packages) are only run there, on a
    runner or in a container: here they are at least read by the shell that will run them."""

    @unittest.skipUnless(POWERSHELL, "PowerShell is needed to read the script")
    def test_the_msi_install_test_is_valid_powershell(self):
        parse = ("$errors = $null; $tokens = $null; "
                 "[void][System.Management.Automation.Language.Parser]::ParseFile($env:SCRIPT_TO_CHECK, [ref]$tokens, [ref]$errors); "
                 "$errors | ForEach-Object { $_.ToString() }; exit [int]($errors.Count -gt 0)")
        result = subprocess.run([POWERSHELL, "-NoProfile", "-NonInteractive", "-Command", parse],
                                env={**os.environ, "SCRIPT_TO_CHECK": str(ROOT / "tests/packages/msi-install.ps1")}, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    @unittest.skipUnless(BASH, "bash is needed to read the scripts")
    def test_the_rpm_install_test_scripts_are_valid_bash(self):
        for name in ("rpm-install.sh", "rpm-container.sh"):
            with self.subTest(script=name):
                result = subprocess.run([BASH, "-n", name], cwd=ROOT / "tests/packages", capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_every_image_the_rpm_test_starts_is_pinned_by_digest(self):
        text = (ROOT / "tests/packages/rpm-install.sh").read_text(encoding="utf-8")
        images = re.findall(r'^\s+"([^"]+@sha256:[^"]*)"$', text, re.M)
        self.assertEqual(len(images), 3, images)
        for image in images:
            self.assertRegex(image, r"^[a-z0-9.\-/]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$")
        self.assertEqual(sorted(image.split("/")[0] for image in images), ["public.ecr.aws", "quay.io", "registry.fedoraproject.org"])


UTIL = "{http://schemas.microsoft.com/wix/UtilExtension}"


class WindowsInstallerSource(unittest.TestCase):
    """What packages/windows/Ghostlink.wxs and scripts/build-windows.ps1 say, read as text: a service
    that restarts after a crash, the desktop app closed on an upgrade, a final that replaces its rc."""

    @classmethod
    def setUpClass(cls):
        cls.product = ElementTree.parse(ROOT / "packages/windows/Ghostlink.wxs").getroot().find(WIX + "Product")
        cls.script = (ROOT / "scripts/build-windows.ps1").read_text(encoding="utf-8")

    def test_a_final_release_can_replace_its_rc_of_the_same_version(self):
        upgrade = self.product.find(WIX + "MajorUpgrade")
        self.assertEqual(upgrade.get("AllowSameVersionUpgrades"), "yes")
        # Older versions are still replaced (before the new files go in), newer ones still refused.
        self.assertEqual(upgrade.get("Schedule"), "afterInstallInitialize")
        self.assertIn("newer version", upgrade.get("DowngradeErrorMessage"))

    def test_the_service_starts_again_after_a_crash_but_not_endlessly(self):
        service = self.product.find(f".//{WIX}ServiceInstall")
        config = self.product.find(f".//{UTIL}ServiceConfig")
        self.assertIsNotNone(config, "the service has no failure actions")
        self.assertEqual(config.get("ServiceName"), service.get("Name"))
        self.assertEqual([config.get(name) for name in ("FirstFailureActionType", "SecondFailureActionType", "ThirdFailureActionType")], ["restart", "restart", "none"])
        self.assertGreater(int(config.get("RestartServiceDelayInSeconds")), 0)
        self.assertGreater(int(config.get("ResetPeriodInDays")), 0)
        # The element sits in the component of the service's file, which the installer configures.
        daemon = next(component for component in self.product.iter(WIX + "Component") if component.find(WIX + "ServiceInstall") is not None)
        self.assertIs(config, daemon.find(UTIL + "ServiceConfig"))

    def test_the_desktop_app_is_closed_before_the_old_version_is_removed(self):
        close = self.product.find(UTIL + "CloseApplication")
        self.assertIsNotNone(close, "a running desktop app is not closed")
        desktop = [file.get("Source") for file in self.product.iter(WIX + "File") if file.get("Id") == "DesktopExe"]
        self.assertEqual(desktop and desktop[0].rsplit("\\", 1)[-1], close.get("Target"))
        self.assertEqual(close.get("CloseMessage"), "yes")
        self.assertTrue(close.get("TerminateProcess"), "an app that stays in the tray is never ended")
        # A prompt would hold up a silent install; a reboot is what the closing is there to avoid.
        self.assertNotEqual(close.get("PromptToContinue"), "yes")
        self.assertEqual(close.get("RebootPrompt"), "no")
        placed = [action for action in self.product.iter(WIX + "Custom") if action.get("Action") == "WixCloseApplications"]
        self.assertEqual([action.get("Before") for action in placed], ["RemoveExistingProducts"])

    def test_the_build_gives_both_wix_tools_the_util_extension_and_silences_only_the_two_meant_warnings(self):
        candle = re.search(r"\$candleArguments = @\((.*)\)", self.script).group(1)
        self.assertIn("'-ext', 'WixUtilExtension'", candle)
        light = re.search(r"^& \(Join-Path \$wixDirectory 'light\.exe'\) (.*)$", self.script, re.M).group(1)
        self.assertIn("-ext WixUtilExtension", light)
        self.assertEqual(re.findall(r"-sice:(\w+)", light), ["ICE61", "ICE63"])
        # The extension comes with the pinned WiX tools (a hash-checked archive): nothing else to fetch.
        self.assertIn("wix314-binaries.zip", (ROOT / "scripts/bootstrap-windows.ps1").read_text(encoding="utf-8"))


DUMP_TABLES = r"""param([string]$Msi, [string]$QueriesFile)
$ErrorActionPreference = 'Stop'
$installer = New-Object -ComObject WindowsInstaller.Installer
$database = $installer.GetType().InvokeMember('OpenDatabase', 'InvokeMethod', $null, $installer, @($Msi, 0))
$index = 0
foreach ($query in Get-Content -LiteralPath $QueriesFile) {
    # A table the MSI does not have (the installer of an older version) has no rows: the test says so.
    try { $view = $database.GetType().InvokeMember('OpenView', 'InvokeMethod', $null, $database, @([string]$query)) }
    catch { $index++; continue }
    $view.GetType().InvokeMember('Execute', 'InvokeMethod', $null, $view, $null) | Out-Null
    $columns = $view.GetType().InvokeMember('ColumnInfo', 'GetProperty', $null, $view, @(0))
    $count = $columns.GetType().InvokeMember('FieldCount', 'GetProperty', $null, $columns, $null)
    while ($true) {
        $record = $view.GetType().InvokeMember('Fetch', 'InvokeMethod', $null, $view, $null)
        if (-not $record) { break }
        $values = @(for ($i = 1; $i -le $count; $i++) { [string]$record.GetType().InvokeMember('StringData', 'GetProperty', $null, $record, @($i)) })
        ConvertTo-Json -InputObject @($index, $values) -Compress
    }
    $view.Close()
    $index++
}
"""


def read_tables(msi, *queries):
    """Rows of the MSI's tables, read through the Windows Installer COM object (no install): one list of
    rows (lists of text) for each SQL query."""
    with tempfile.TemporaryDirectory() as temporary:
        script, queries_file = Path(temporary) / "dump.ps1", Path(temporary) / "queries.txt"
        script.write_text(DUMP_TABLES, encoding="utf-8")
        queries_file.write_text("\n".join(queries) + "\n", encoding="utf-8")
        result = subprocess.run([POWERSHELL, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(script),
                                 "-Msi", str(msi), "-QueriesFile", str(queries_file)], capture_output=True, text=True)
    if result.returncode != 0:
        raise AssertionError(f"could not read {msi}: {result.stderr.strip()[:500]}")
    tables = [[] for _ in queries]
    for line in result.stdout.splitlines():
        if line.startswith("["):
            index, row = json.loads(line)
            tables[index].append(row)
    return tables


def build_installer(directory, version):
    """The real scripts/build-windows.ps1 (without compiling the programs) in a scratch tree with stand-in
    executables and the pinned WiX tools, as the release does it. Returns the MSI."""
    tree = Path(directory) / "tree"
    for relative in ("scripts/build-windows.ps1", "scripts/cargo.ps1", "packages/windows/Ghostlink.wxs", "apps/desktop/src-tauri/icons/icon.ico",
                     "LICENSE", "NOTICE.md", "LICENSES/GPL-3.0.txt"):
        (tree / relative).parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / relative, tree / relative)
    for relative in ("target/release/ghostlink.exe", "target/release/ghostlinkd.exe", "target/release/ghostlink-mcp.exe",
                     "target/release/ghostlink-tunnel.exe", "apps/desktop/src-tauri/target/release/ghostlink-desktop.exe"):
        (tree / relative).parent.mkdir(parents=True, exist_ok=True)
        (tree / relative).write_bytes(b"stand-in")
    (tree / ".tools").mkdir()
    # A junction (no privilege needed) rather than a copy of 117 MB of tools.
    subprocess.run(["cmd", "/c", "mklink", "/J", str(tree / ".tools/wix314"), str(ROOT / ".tools/wix314")], check=True, capture_output=True)
    try:
        result = subprocess.run([POWERSHELL, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", str(tree / "scripts/build-windows.ps1"),
                                 "-SkipCompile", "-Version", version], capture_output=True, text=True)
    finally:
        os.rmdir(tree / ".tools/wix314")  # the junction only, never what it points to
    if result.returncode != 0:
        raise AssertionError(f"build-windows.ps1 failed:\n{result.stdout[-2000:]}\n{result.stderr[-2000:]}")
    # A warning from candle or light (an ICE that is not one of the two meant ones) is a finding.
    warnings = [line for line in result.stdout.splitlines() if re.search(r"warning (CNDL|LGHT)\d+", line)]
    if warnings:
        raise AssertionError("the installer build warns:\n" + "\n".join(warnings))
    return tree / f"artifacts/windows-x64/Ghostlink-{version}-windows-x64.msi"


@unittest.skipUnless(os.name == "nt" and POWERSHELL, "the MSI is built and read on Windows")
class WindowsInstallerTables(unittest.TestCase):
    """The tables of the compiled MSI, with no install: the one the release built when
    GHOSTLINK_TEST_MSI names it, otherwise one built here with the pinned WiX tools (skipped without
    them: scripts/bootstrap-windows.ps1)."""

    @classmethod
    def setUpClass(cls):
        given = os.environ.get("GHOSTLINK_TEST_MSI")
        if given:
            cls.msi = Path(given).resolve()
            assert cls.msi.is_file(), f"GHOSTLINK_TEST_MSI is not a file: {given}"
        else:
            if not (ROOT / ".tools/wix314/candle.exe").is_file():
                raise unittest.SkipTest("the pinned WiX tools are not fetched (scripts/bootstrap-windows.ps1)")
            cls.temporary = tempfile.TemporaryDirectory()
            cls.addClassCleanup(cls.temporary.cleanup)
            cls.msi = build_installer(cls.temporary.name, "0.9.2-rc.1")
        (cls.upgrade, cls.version, cls.service_config, cls.close, cls.service, cls.sequence, cls.conditions, cls.control) = read_tables(
            cls.msi,
            "SELECT UpgradeCode, VersionMin, VersionMax, Attributes, ActionProperty FROM Upgrade",
            "SELECT Property, Value FROM Property WHERE Property='ProductVersion' OR Property='UpgradeCode'",
            "SELECT ServiceName, Component_, NewService, FirstFailureActionType, SecondFailureActionType, ThirdFailureActionType, ResetPeriodInDays, RestartServiceDelayInSeconds FROM ServiceConfig",
            "SELECT Target, TerminateExitCode FROM WixCloseApplication",
            "SELECT Name, StartName, StartType FROM ServiceInstall",
            "SELECT Action, Sequence FROM InstallExecuteSequence",
            "SELECT Condition FROM LaunchCondition",
            "SELECT Name, Component_ FROM ServiceControl")
        cls.properties = dict(cls.version)
        cls.order = {action: int(sequence) for action, sequence in cls.sequence}

    # Attributes of an Upgrade row: 1 migrate features, 2 only detect, 256 minimum inclusive, 512 maximum inclusive.
    def test_a_product_of_the_same_version_or_older_is_replaced_and_a_newer_one_stops_the_install(self):
        replaced = [row for row in self.upgrade if row[4] == "WIX_UPGRADE_DETECTED"]
        refused = [row for row in self.upgrade if row[4] == "WIX_DOWNGRADE_DETECTED"]
        self.assertEqual((len(replaced), len(refused)), (1, 1), self.upgrade)
        version = self.properties["ProductVersion"]
        code, minimum, maximum, attributes, _ = replaced[0]
        self.assertEqual((code, minimum, maximum), ("{" + self.properties["UpgradeCode"].strip("{}") + "}", "", version))
        self.assertTrue(int(attributes) & 512, "an installed product of this very version (the rc) is not replaced")
        self.assertFalse(int(attributes) & 2, "the older products are only detected, never removed")
        code, minimum, maximum, attributes, _ = refused[0]
        self.assertEqual((minimum, maximum), (version, ""))
        self.assertEqual(int(attributes) & ~1, 2, "only a version above this one (minimum not inclusive) is a downgrade")
        self.assertIn(["NOT WIX_DOWNGRADE_DETECTED"], self.conditions)

    def test_the_service_is_set_to_restart_after_a_crash(self):
        self.assertEqual(self.service_config, [["Ghostlink", "Daemon", "0", "restart", "restart", "none", "1", "10"]])
        # The same service the installer creates, as the account that owns the machine's network.
        self.assertEqual(self.service, [["Ghostlink", "LocalSystem", "2"]])
        self.assertEqual([row[0] for row in self.control], ["Ghostlink"])
        # It is configured after the service exists and before it is started, and not when removing it.
        self.assertLess(self.order["InstallServices"], self.order["SchedServiceConfig"])
        self.assertLess(self.order["SchedServiceConfig"], self.order["StartServices"])

    def test_the_desktop_app_is_closed_after_the_install_begins_and_before_anything_is_removed(self):
        self.assertEqual(self.close, [["ghostlink-desktop.exe", "1"]])
        self.assertLess(self.order["InstallInitialize"], self.order["WixCloseApplications"])
        self.assertLess(self.order["WixCloseApplications"], self.order["RemoveExistingProducts"])
        self.assertLess(self.order["RemoveExistingProducts"], self.order["StopServices"])
        self.assertLess(self.order["RemoveExistingProducts"], self.order["InstallFiles"])


if __name__ == "__main__":
    unittest.main()
