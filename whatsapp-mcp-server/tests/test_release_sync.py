"""The release must be bumped everywhere at once: the downgrade guard compares them."""

import re
import tomllib
from pathlib import Path

import bridge_manager

ROOT = Path(__file__).resolve().parent.parent
PAIRING_GO = ROOT.parent / "whatsapp-bridge" / "pairing.go"


def _go_string(name: str) -> str:
    match = re.search(rf'\b{name}\s*=\s*"([^"]+)"', PAIRING_GO.read_text(encoding="utf-8"))
    assert match, f"{name} not found in {PAIRING_GO}"
    return match.group(1)


def test_release_constants_match():
    pyproject = tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))
    assert pyproject["project"]["version"] == bridge_manager.RELEASE
    assert _go_string("bridgeRelease") == bridge_manager.RELEASE
    assert _go_string("bridgeVersion") == bridge_manager.RELEASE
