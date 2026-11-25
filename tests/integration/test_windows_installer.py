"""Who owns the Windows service after a first install and after an upgrade.

An install test in CI does not reach this, so it is checked with a small model of how Windows
Installer fills GHOSTLINK_OWNER_SID from packages/windows/Ghostlink.wxs.
"""
from pathlib import Path
import re
import shutil
import subprocess
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


if __name__ == "__main__":
    unittest.main()
