"""Start the MCP server over stdio and dump tool names + input schemas as sorted JSON.

Usage: uv run scripts/dump_tools.py [output.json]   (prints to stdout without an argument)
"""

import asyncio
import json
import sys
from pathlib import Path

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

SERVER_DIR = Path(__file__).resolve().parent.parent


async def dump_tools() -> dict:
    params = StdioServerParameters(
        command=sys.executable, args=[str(SERVER_DIR / "main.py")], cwd=str(SERVER_DIR)
    )
    async with stdio_client(params) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            result = await session.list_tools()
    return {t.name: t.inputSchema for t in sorted(result.tools, key=lambda t: t.name)}


def main() -> int:
    text = json.dumps(asyncio.run(dump_tools()), indent=2, sort_keys=True) + "\n"
    if len(sys.argv) > 1:
        Path(sys.argv[1]).write_text(text)
    else:
        sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
