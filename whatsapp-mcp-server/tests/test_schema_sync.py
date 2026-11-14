"""The test schema in conftest.py must match the bridge's index, FTS and trigger DDL."""

import re

import pytest
from conftest import SCHEMA, SERVER_DIR

STORE_GO = SERVER_DIR.parent / "whatsapp-bridge" / "store.go"


def _squash(sql: str) -> str:
    return " ".join(sql.split())


@pytest.mark.parametrize(
    "const", ["migrateIndexesSQL", "migrateFTSTableSQL", "migrateFTSTriggersSQL"]
)
def test_conftest_schema_matches_bridge_ddl(const):
    match = re.search(rf"const {const} = `([^`]*)`", STORE_GO.read_text())
    assert match, f"{const} not found in {STORE_GO}"
    assert _squash(match.group(1)) in _squash(SCHEMA), f"conftest SCHEMA drifted from {const}"
