"""scripts/dump_tools.py: dumps the live server's tool schemas (real stdio subprocess)."""

import importlib.util
import json
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parent.parent / "scripts" / "dump_tools.py"
BASELINE = json.loads((Path(__file__).parent / "baseline_tools.json").read_text())


@pytest.fixture(scope="module")
def dump_tools():
    spec = importlib.util.spec_from_file_location("dump_tools", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


async def test_dump_tools_lists_every_server_tool(dump_tools):
    # Autostart is off (conftest), so the spawned server never starts a bridge.
    tools = await dump_tools.dump_tools()
    assert list(tools) == sorted(tools)
    assert set(BASELINE) <= set(tools)
    assert {"get_connection_status", "get_pairing_qr", "pair_with_phone_number"} <= set(tools)
    assert len(tools) == 15


def test_main_writes_file_or_stdout(dump_tools, monkeypatch, tmp_path, capsys):
    async def fake_dump():
        return {"b": {"x": 1}, "a": {}}

    monkeypatch.setattr(dump_tools, "dump_tools", fake_dump)
    out = tmp_path / "tools.json"
    monkeypatch.setattr(sys, "argv", ["dump_tools.py", str(out)])
    assert dump_tools.main() == 0
    assert json.loads(out.read_text()) == {"a": {}, "b": {"x": 1}}
    assert capsys.readouterr().out == ""

    monkeypatch.setattr(sys, "argv", ["dump_tools.py"])
    assert dump_tools.main() == 0
    assert json.loads(capsys.readouterr().out) == {"a": {}, "b": {"x": 1}}
