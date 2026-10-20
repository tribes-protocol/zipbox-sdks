"""A LangChain tool that runs shell commands in a Zipbox box.

pip install zipbox langchain-core langchain-openai
"""

from langchain_core.tools import tool
from langchain_openai import ChatOpenAI

from zipbox import Zipbox

zb = Zipbox()
box = zb.sandboxes.get_or_create("langchain-agent")


@tool
def run_in_box(command: str) -> str:
    """Run a bash command in a persistent Linux box and return its output."""
    result = box.exec(command)
    output = (result.stdout + result.stderr)[-8000:]
    return f"exit {result.exit_code}\n{output}"


if __name__ == "__main__":
    llm = ChatOpenAI(model="gpt-4o-mini").bind_tools([run_in_box])
    reply = llm.invoke("What Linux distribution is the box running? Use the tool.")
    for call in reply.tool_calls:
        print(run_in_box.invoke(call["args"]))
