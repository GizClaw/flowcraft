"""A plugin-provided graph node, written in Python.

The craft design promises that a plugin node is "just an ordinary MCP
tool handler" on the plugin side; this file is the smallest thing that
honours the contract: read {node, config}, return {writes}.

It is served with `uvx --from mcp[cli] python server.py`, which is why
the import tolerates both SDK generations (mcp 2 renamed FastMCP to
MCPServer and kept the decorator API).
"""

try:  # mcp >= 2
    from mcp.server.mcpserver import MCPServer
except ImportError:  # mcp < 2
    from mcp.server.fastmcp import FastMCP as MCPServer

server = MCPServer("python-node-plugin")


@server.tool()
def node_echo(node: dict, config: dict) -> dict:
    """Echo the node id back through the node writes contract."""
    greeting = config.get("greeting", "")
    return {"writes": {"result": "echo:" + str(node.get("id")),
                       "greeting": greeting}}


if __name__ == "__main__":
    server.run()
