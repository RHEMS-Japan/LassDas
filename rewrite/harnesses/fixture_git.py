"""Test-only Git settings installed before init/clone starts writing objects.

The local config also reaches receive-pack, so deleting a fixture cannot race
detached maintenance. Neither product commands nor personal config are edited.
The environment also covers product-created workspaces that disable templates.
"""
from pathlib import Path


def fixture_git_environment(environment):
    values = dict(environment, GIT_TEMPLATE_DIR=str(Path(__file__).with_name("fixtures") / "git"))
    index = int(values.get("GIT_CONFIG_COUNT", "0"))
    values["GIT_CONFIG_COUNT"] = str(index + 1)
    values["GIT_CONFIG_KEY_" + str(index)] = "maintenance.autoDetach"
    values["GIT_CONFIG_VALUE_" + str(index)] = "false"
    return values
